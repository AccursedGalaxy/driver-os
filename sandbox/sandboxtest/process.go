package sandboxtest

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// ProcessFactory builds a fresh ProcessHost rooted at dir for one subtest, plus a
// cleanup (may be nil). Mirrors Factory/SessionFactory.
type ProcessFactory func(t *testing.T, dir string) (ph sandbox.ProcessHost, cleanup func())

// RunProcessConformance runs the shared ProcessHost contract against whatever
// backend `newHost` produces: a long-lived process echoes stdin→stdout, Kill
// terminates it and Wait then returns, a self-exiting process makes Wait return,
// and stderr is captured into the bounded snapshot without ever deadlocking the
// process. Every wait/read is bounded so a contract violation FAILS rather than
// hangs CI. Both backends must pass identically (local always; docker tagged).
func RunProcessConformance(t *testing.T, newHost ProcessFactory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, ph sandbox.ProcessHost)
	}{
		{"EchoRoundTrip", processEchoRoundTrip},
		{"KillTerminatesAndWaitReturns", processKillTerminates},
		{"SelfExitMakesWaitReturn", processSelfExit},
		{"StderrCapturedWithoutDeadlock", processStderrCaptured},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ph, cleanup := newHost(t, dir)
			if cleanup != nil {
				defer cleanup()
			}
			c.fn(t, ph)
		})
	}
}

// waitWithin runs p.Wait() and fails if it doesn't return within d — the guard
// that turns "Kill/exit was never observed" from a CI hang into a clear failure.
func waitWithin(t *testing.T, p sandbox.Process, d time.Duration, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s: Wait did not return within %s", what, d)
	}
}

// readLineWithin reads one '\n'-terminated line from r, failing if none arrives in
// time (so a process that never echoes fails instead of hanging).
func readLineWithin(t *testing.T, r io.Reader, d time.Duration, what string) string {
	t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		ch <- res{line, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil && got.line == "" {
			t.Fatalf("%s: read failed: %v", what, got.err)
		}
		return strings.TrimSpace(got.line)
	case <-time.After(d):
		t.Fatalf("%s: no output within %s", what, d)
		return ""
	}
}

func processEchoRoundTrip(t *testing.T, ph sandbox.ProcessHost) {
	// `cat` is the canonical echo-stdin-to-stdout long-lived process.
	p, err := ph.StartProcess(context.Background(), sandbox.Command{Path: "cat"})
	if err != nil {
		t.Fatalf("StartProcess(cat): %v", err)
	}
	defer p.Kill()

	if _, err := io.WriteString(p.Stdin(), "ping\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if got := readLineWithin(t, p.Stdout(), 5*time.Second, "cat echo"); got != "ping" {
		t.Errorf("stdout = %q, want ping", got)
	}
}

func processKillTerminates(t *testing.T, ph sandbox.ProcessHost) {
	// A process that would otherwise block forever; Kill must end it and Wait return.
	p, err := ph.StartProcess(context.Background(), sandbox.Command{Path: "sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatalf("StartProcess(sleep): %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitWithin(t, p, 10*time.Second, "after Kill")
	// Idempotent: a second Kill is harmless.
	if err := p.Kill(); err != nil {
		t.Errorf("second Kill should be a no-op, got: %v", err)
	}
}

func processSelfExit(t *testing.T, ph sandbox.ProcessHost) {
	// A process that exits on its own: Wait must observe it without a Kill.
	p, err := ph.StartProcess(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer p.Kill()
	waitWithin(t, p, 10*time.Second, "self-exit")
}

func processStderrCaptured(t *testing.T, ph sandbox.ProcessHost) {
	// Writes to stderr, then `cat` keeps the process alive on stdin — so we can prove
	// stderr was captured into the snapshot WHILE the process still runs (i.e. the
	// drain didn't block it), and that the process is still responsive afterward.
	p, err := ph.StartProcess(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "echo oops >&2; exec cat"}})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer p.Kill()

	// The drain is async; poll the snapshot briefly.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(string(p.StderrSnapshot()), "oops") {
		if time.Now().After(deadline) {
			t.Fatalf("stderr snapshot never captured %q (got %q)", "oops", p.StderrSnapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Still alive and responsive (stderr draining didn't deadlock it).
	if _, err := io.WriteString(p.Stdin(), "alive\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if got := readLineWithin(t, p.Stdout(), 5*time.Second, "post-stderr echo"); got != "alive" {
		t.Errorf("process not responsive after stderr write: stdout = %q, want alive", got)
	}
}
