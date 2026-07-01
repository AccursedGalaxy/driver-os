// Package proc implements sandbox.Process — the live-stream handle a ProcessHost
// hands back — once, for ANY backend. A long-lived process looks the same to its
// driver whether it runs as a host child (local) or inside a container reached via
// `docker exec` (docker): bytes in on stdin, bytes out on stdout, stderr drained
// to a bounded tail, and a memoized way to wait for or force its exit. The only
// per-backend parts are HOW to wait for and HOW to kill the process, which the
// backend supplies as closures.
//
// See ../../docs/specs/SESSION.md (slice 2) for the design and the os/exec stream-plumbing
// rationale (explicit os.Pipe, caller-owned ends).
package proc

import (
	"io"
	"sync"
)

// DefaultStderrCap is the bound on retained stderr: the last 64 KiB. Enough to
// hold a crash's final log lines without letting a chatty server's stderr grow
// unbounded in memory (P4).
const DefaultStderrCap = 64 << 10

// Process is the shared sandbox.Process. Construct it with New; the backend owns
// only the wait/kill closures and the three stream ends.
type Process struct {
	stdin  io.Writer
	stdout io.Reader
	stderr *ring

	waitDone chan struct{} // closed once the process has been reaped.
	waitErr  error         // the reap result; valid after waitDone is closed.

	killOnce sync.Once
	killErr  error
	killFn   func() error
}

var _ interface {
	Stdin() io.Writer
	Stdout() io.Reader
	StderrSnapshot() []byte
	Wait() error
	Kill() error
} = (*Process)(nil)

// New builds a Process over a started backend process. stdin/stdout are the live
// streams handed to the driver. stderrSrc is drained in the background into a
// bounded ring (last maxStderr bytes) and closed when it reaches EOF, so the
// process never stalls on a full stderr pipe. waitFn blocks until the process
// exits and returns its status; it is run ONCE in the background (so the process
// is always reaped, even if the driver never calls Wait) and its result memoized.
// killFn terminates the process; it is invoked at most once.
func New(stdin io.Writer, stdout, stderrSrc io.Reader, maxStderr int, waitFn, killFn func() error) *Process {
	p := &Process{
		stdin:    stdin,
		stdout:   stdout,
		stderr:   newRing(maxStderr),
		waitDone: make(chan struct{}),
		killFn:   killFn,
	}
	go func() {
		_, _ = io.Copy(p.stderr, stderrSrc)
		if c, ok := stderrSrc.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	go func() {
		p.waitErr = waitFn()
		close(p.waitDone)
	}()
	return p
}

func (p *Process) Stdin() io.Writer       { return p.stdin }
func (p *Process) Stdout() io.Reader      { return p.stdout }
func (p *Process) StderrSnapshot() []byte { return p.stderr.snapshot() }

// Wait blocks until the background reaper has observed the exit, then returns its
// (memoized) result. Safe to call any number of times, from any goroutine.
func (p *Process) Wait() error {
	<-p.waitDone
	return p.waitErr
}

// Kill terminates the process via the backend's closure, exactly once.
func (p *Process) Kill() error {
	p.killOnce.Do(func() { p.killErr = p.killFn() })
	return p.killErr
}

// Done is closed once the process has exited and been reaped. It lets a caller
// (e.g. a ctx-cancel watcher in the backend) wait for natural death without
// parking a goroutine in Wait. Not part of sandbox.Process — an extra the backends
// use during wiring.
func (p *Process) Done() <-chan struct{} { return p.waitDone }

// ring is a bounded, concurrency-safe byte sink keeping only the most recent max
// bytes — the stderr TAIL, which is what matters after a crash. max <= 0 means
// unbounded.
type ring struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if r.max > 0 && len(r.buf) > r.max {
		// Keep the last max bytes, copying into a fresh slice so the backing array
		// can't grow without bound across many over-limit writes.
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.max:]...)
	}
	return len(p), nil
}

func (r *ring) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf...)
}
