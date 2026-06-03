package sandboxtest

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// SessionFactory builds a fresh Sessioner rooted at dir for one subtest, plus a
// cleanup (may be nil). It mirrors Factory but returns the Sessioner capability
// directly, so the suite can open Sessions without re-type-asserting in every
// backend's test.
type SessionFactory func(t *testing.T, dir string) (sr sandbox.Sessioner, cleanup func())

// RunSessionConformance runs the shared Session contract against whatever backend
// `newSandbox` produces: cwd and env persist across Exec calls, independent
// sessions are isolated, a non-shell command passes through, and an explicit
// cmd.Dir overrides the session cwd. Both backends must pass it identically (local
// always; docker behind the docker_integration tag).
func RunSessionConformance(t *testing.T, newSandbox SessionFactory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, sr sandbox.Sessioner)
	}{
		{"CwdPersistsAcrossExec", cwdPersistsAcrossExec},
		{"EnvPersistsAcrossExec", envPersistsAcrossExec},
		{"SessionsAreIsolated", sessionsAreIsolated},
		{"NonShellCommandPassesThrough", nonShellCommandPassesThrough},
		{"ExplicitDirOverridesSessionCwd", explicitDirOverridesSessionCwd},
		{"NonZeroExitIsResultNotError", sessionNonZeroExitIsResultNotError},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			sr, cleanup := newSandbox(t, dir)
			if cleanup != nil {
				defer cleanup()
			}
			c.fn(t, sr)
		})
	}
}

// openSession opens a Session and registers its Close.
func openSession(t *testing.T, sr sandbox.Sessioner) sandbox.Session {
	t.Helper()
	sess, err := sr.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// sh runs a shell command in the session and returns trimmed stdout, failing on a
// machinery error (a non-zero exit is fine — the caller asserts on output).
func sh(t *testing.T, sess sandbox.Session, command string) string {
	t.Helper()
	res, err := sess.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", command}})
	if err != nil {
		t.Fatalf("session Exec %q: %v", command, err)
	}
	return strings.TrimSpace(string(res.Stdout))
}

func cwdPersistsAcrossExec(t *testing.T, sr sandbox.Sessioner) {
	sess := openSession(t, sr)
	// Make a subdir and cd into it in one call; a SEPARATE later call must still be
	// there — that is the whole point of a stateful session.
	sh(t, sess, "mkdir -p subdir && cd subdir")
	if got := sh(t, sess, "pwd"); !strings.HasSuffix(got, "/subdir") {
		t.Errorf("cwd did not persist: pwd = %q, want a path ending in /subdir", got)
	}
}

func envPersistsAcrossExec(t *testing.T, sr sandbox.Sessioner) {
	sess := openSession(t, sr)
	sh(t, sess, "export GREETING=hello")
	if got := sh(t, sess, `echo "$GREETING"`); got != "hello" {
		t.Errorf("export did not persist: echo $GREETING = %q, want hello", got)
	}
}

func sessionsAreIsolated(t *testing.T, sr sandbox.Sessioner) {
	a := openSession(t, sr)
	b := openSession(t, sr)
	sh(t, a, "export SECRET=in_a")
	if got := sh(t, b, `echo "${SECRET:-unset}"`); got != "unset" {
		t.Errorf("session B saw session A's env: SECRET = %q, want unset", got)
	}
}

func nonShellCommandPassesThrough(t *testing.T, sr sandbox.Sessioner) {
	sess := openSession(t, sr)
	// A raw (non `sh -c`) command can't carry session state, but it must still run
	// and return its Result unwrapped.
	res, err := sess.Exec(context.Background(), sandbox.Command{Path: "echo", Args: []string{"raw"}})
	if err != nil {
		t.Fatalf("non-shell Exec: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "raw" {
		t.Errorf("non-shell command stdout = %q, want raw", got)
	}
}

func explicitDirOverridesSessionCwd(t *testing.T, sr sandbox.Sessioner) {
	sess := openSession(t, sr)
	// Establish a session cwd, then run a command with an explicit Dir: it must run
	// in Dir, not the saved cwd.
	sh(t, sess, "mkdir -p a b && cd a")
	res, err := sess.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "pwd"}, Dir: "b",
	})
	if err != nil {
		t.Fatalf("Exec with Dir: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); !strings.HasSuffix(got, "/b") {
		t.Errorf("explicit Dir ignored: pwd = %q, want a path ending in /b", got)
	}
}

func sessionNonZeroExitIsResultNotError(t *testing.T, sr sandbox.Sessioner) {
	sess := openSession(t, sr)
	res, err := sess.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "exit 7"}})
	if err != nil {
		t.Fatalf("non-zero exit returned a Go error (P6 violation): %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit = %d, want 7 (the wrapper must propagate the command's code)", res.ExitCode)
	}
}
