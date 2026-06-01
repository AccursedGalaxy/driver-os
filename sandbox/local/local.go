// Package local is the IsolationNone sandbox backend: it runs commands as child
// processes on the host and confines file access to a root directory with a path
// fence. It is the lifted, generalized form of cmd/agent's old confineToRoot.
//
// IsolationNone means there is NO real isolation for Exec — a shell command runs
// on the host with the agent's own privileges; the fence only governs the
// ReadFile/WriteFile/ListDir paths, not what a spawned process can touch. So this
// backend is for TRUSTED code only (code we wrote). Running untrusted,
// model-authored code requires a stronger backend (container/gVisor/microVM)
// behind the same sandbox.Sandbox interface — see ../../SANDBOX.md.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Sandbox is a host-local sandbox confined to a single root directory.
type Sandbox struct {
	root string // absolute, cleaned
}

// compile-time proof we satisfy the interface(s).
var (
	_ sandbox.Sandbox       = (*Sandbox)(nil)
	_ sandbox.LimitedReader = (*Sandbox)(nil)
)

// New creates a local sandbox rooted at dir. dir must exist and be a directory;
// it is resolved to an absolute, cleaned path that becomes the fence boundary.
func New(dir string) (*Sandbox, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sandbox root %q is not a directory", abs)
	}
	return &Sandbox{root: abs}, nil
}

// Capabilities reports the truth: no isolation, host network. The runner can use
// this to refuse running untrusted code here.
func (s *Sandbox) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Isolation: sandbox.IsolationNone, Network: true}
}

// resolve maps a sandbox-relative path to an absolute host path, refusing any
// path that escapes the root. This IS the fence. Because s.root is absolute,
// filepath.Join yields an absolute, cleaned path; a "../" escape resolves above
// the root and is rejected by the prefix check.
func (s *Sandbox) resolve(path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs := filepath.Join(s.root, path)
	if abs != s.root && !strings.HasPrefix(abs, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("refused: %q is outside the sandbox root %s", path, s.root)
	}
	return abs, nil
}

// Exec runs a command to completion. A non-zero exit (or a Timeout kill) is a
// normal *Result, not an error — only a genuine failure to start the command is
// returned as err (Principle 6: failures are observations).
func (s *Sandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	dir := s.root
	if cmd.Dir != "" {
		d, err := s.resolve(cmd.Dir)
		if err != nil {
			return nil, err
		}
		dir = d
	}

	runCtx := ctx
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(runCtx, cmd.Path, cmd.Args...)
	c.Dir = dir
	c.Env = append(os.Environ(), cmd.Env...)
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	start := time.Now()
	err := c.Run()
	res := &sandbox.Result{
		ExitCode: -1,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: time.Since(start),
	}
	if c.ProcessState != nil {
		res.ExitCode = c.ProcessState.ExitCode()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}

	if err != nil {
		// Non-zero exit (incl. a timeout-killed process) is a Result the caller
		// observes — not a Go error. Only "couldn't run it at all" is an error.
		var ee *exec.ExitError
		if errors.As(err, &ee) || res.TimedOut {
			return res, nil
		}
		return res, fmt.Errorf("exec %q: %w", cmd.Path, err)
	}
	return res, nil
}

// ReadFile reads a file within the fence.
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	abs, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// ReadFileLimit implements sandbox.LimitedReader: a bounded read that never pulls
// more than max bytes into memory (Principle 4). It reads max+1 bytes — one past
// the limit — to detect whether the file continued, without copying the tail.
func (s *Sandbox) ReadFileLimit(ctx context.Context, path string, max int64) ([]byte, bool, error) {
	abs, err := s.resolve(path)
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if max <= 0 {
		data, err := io.ReadAll(f) // "no limit" == ReadFile.
		return data, false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// WriteFile writes a file within the fence (parent directories must already exist).
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	abs, err := s.resolve(path)
	if err != nil {
		return err
	}
	return os.WriteFile(abs, data, mode)
}

// ListDir lists a directory within the fence.
func (s *Sandbox) ListDir(ctx context.Context, path string) ([]sandbox.DirEntry, error) {
	abs, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, sandbox.DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

// Close releases resources. The local backend holds none, so this is a no-op —
// but callers should always Close so swapping in a container/VM backend (which
// very much does need teardown) is a drop-in change.
func (s *Sandbox) Close() error { return nil }
