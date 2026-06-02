// Package sandboxtest holds the behavioral conformance suite for any
// sandbox.Sandbox backend. It is the safety net that lets the local and docker
// backends stay behaviorally IDENTICAL: a single table of assertions about the
// shared contract (non-zero exit is a Result not an error; Timeout sets TimedOut;
// the file fence works; a "../" escape is refused), run against whatever backend
// a caller hands in.
//
// It lives in a non-test .go file (not _test.go) on purpose, so it can be
// imported and invoked from BOTH sandbox/local's tests (always, no daemon) and
// sandbox/docker's integration tests (behind the docker_integration tag). The
// only dependency is the standard testing package, which is import-safe here.
package sandboxtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Factory builds a fresh Sandbox rooted at dir for one subtest, plus a cleanup.
// dir is a writable directory the suite pre-populates and treats as the fence
// root. The returned cleanup (may be nil) is deferred after the subtest — a
// container backend closes its container there.
type Factory func(t *testing.T, dir string) (sb sandbox.Sandbox, cleanup func())

// RunConformance runs the shared contract suite against the backend `newSandbox`
// produces. Call it from a backend's test with a factory that constructs that
// backend. Each case gets its own freshly-rooted sandbox via the factory.
func RunConformance(t *testing.T, newSandbox Factory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, sb sandbox.Sandbox, root string)
	}{
		{"ExecEchoSucceeds", execEchoSucceeds},
		{"NonZeroExitIsResultNotError", nonZeroExitIsResultNotError},
		{"TimeoutSetsTimedOut", timeoutSetsTimedOut},
		{"WriteReadRoundTrip", writeReadRoundTrip},
		{"ListDirSeesWrites", listDirSeesWrites},
		{"FenceRefusesReadEscape", fenceRefusesReadEscape},
		{"FenceRefusesWriteEscape", fenceRefusesWriteEscape},
		{"FenceRefusesSymlinkReadEscape", fenceRefusesSymlinkReadEscape},
		{"FenceRefusesSymlinkWriteEscape", fenceRefusesSymlinkWriteEscape},
		{"ExecStdinIsForwarded", execStdinIsForwarded},
		{"ExecHonorsWorkingDir", execHonorsWorkingDir},
		{"ExecRefusesDirEscape", execRefusesDirEscape},
		{"CapabilitiesAdvertised", capabilitiesAdvertised},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			sb, cleanup := newSandbox(t, dir)
			if cleanup != nil {
				defer cleanup()
			}
			c.fn(t, sb, dir)
		})
	}
}

func ctx() context.Context { return context.Background() }

func execEchoSucceeds(t *testing.T, sb sandbox.Sandbox, _ string) {
	res, err := sb.Exec(ctx(), sandbox.Command{Path: "sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "hi" {
		t.Errorf("stdout = %q, want hi", got)
	}
}

func nonZeroExitIsResultNotError(t *testing.T, sb sandbox.Sandbox, _ string) {
	res, err := sb.Exec(ctx(), sandbox.Command{Path: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatalf("non-zero exit returned a Go error (P6 violation): %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("a plain non-zero exit must not be marked TimedOut")
	}
}

func timeoutSetsTimedOut(t *testing.T, sb sandbox.Sandbox, _ string) {
	res, err := sb.Exec(ctx(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "sleep 30"}, Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (a Timeout must kill the command)")
	}
}

func writeReadRoundTrip(t *testing.T, sb sandbox.Sandbox, _ string) {
	if err := sb.WriteFile(ctx(), "data.txt", []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := sb.ReadFile(ctx(), "data.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("round trip = %q, want payload", data)
	}
}

func listDirSeesWrites(t *testing.T, sb sandbox.Sandbox, _ string) {
	if err := sb.WriteFile(ctx(), "a.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := sb.ListDir(ctx(), ".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "a.txt" && !e.IsDir {
			found = true
		}
	}
	if !found {
		t.Errorf("ListDir did not see the written file: %+v", entries)
	}
}

func fenceRefusesReadEscape(t *testing.T, sb sandbox.Sandbox, _ string) {
	for _, p := range []string{"../escape.txt", "../../etc/passwd"} {
		if _, err := sb.ReadFile(ctx(), p); err == nil {
			t.Errorf("ReadFile(%q) = nil error, want fence refusal", p)
		}
	}
}

func fenceRefusesWriteEscape(t *testing.T, sb sandbox.Sandbox, _ string) {
	if err := sb.WriteFile(ctx(), "../escape.txt", []byte("x"), 0o644); err == nil {
		t.Error("WriteFile escaped the fence, want refusal")
	}
}

// fenceRefusesSymlinkReadEscape is the confused-deputy guard: a symlink planted
// INSIDE the workspace pointing OUTSIDE it (what hostile in-container code can do
// to a writable bind mount) must not let a host-side ReadFile follow it off-root.
// The symlink is created host-side here, but the fence can't tell who made it —
// which is exactly the point.
func fenceRefusesSymlinkReadEscape(t *testing.T, sb sandbox.Sandbox, root string) {
	secretDir := t.TempDir() // a sibling of root, outside the fence.
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leak")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	data, err := sb.ReadFile(ctx(), "leak")
	if err == nil {
		t.Errorf("ReadFile followed a symlink out of the fence and read %q; want refusal", data)
	}
}

// fenceRefusesSymlinkWriteEscape is the write-side confused deputy: a symlinked
// directory inside the workspace must not let a WriteFile land outside it.
func fenceRefusesSymlinkWriteEscape(t *testing.T, sb sandbox.Sandbox, root string) {
	outDir := t.TempDir()
	if err := os.Symlink(outDir, filepath.Join(root, "escape-dir")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	if err := sb.WriteFile(ctx(), "escape-dir/pwned.txt", []byte("x"), 0o644); err == nil {
		if _, statErr := os.Stat(filepath.Join(outDir, "pwned.txt")); statErr == nil {
			t.Error("WriteFile wrote THROUGH a symlinked dir to outside the fence; want refusal")
		}
	}
}

func execStdinIsForwarded(t *testing.T, sb sandbox.Sandbox, _ string) {
	res, err := sb.Exec(ctx(), sandbox.Command{Path: "cat", Stdin: []byte("piped-input")})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "piped-input" {
		t.Errorf("cat stdout = %q, want piped-input", got)
	}
}

func execHonorsWorkingDir(t *testing.T, sb sandbox.Sandbox, _ string) {
	if err := sb.WriteFile(ctx(), "marker.txt", []byte("here"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Run in "." and confirm the working dir is the workspace root (the marker is
	// visible to a relative `cat`). This keeps the assertion backend-agnostic: the
	// local backend reports a host path, the container an in-container path, but
	// both must run with the workspace as cwd.
	res, err := sb.Exec(ctx(), sandbox.Command{Path: "sh", Args: []string{"-c", "cat marker.txt"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "here" {
		t.Errorf("cwd not the workspace root: cat marker.txt = %q (exit %d, stderr %s)", got, res.ExitCode, res.Stderr)
	}
}

// execRefusesDirEscape proves cmd.Dir is fenced like a path: a working directory
// that escapes the root must be refused, not honored — otherwise a command could
// be aimed outside the workspace. Both backends must reject it (the docker backend
// reuses the same host-side fence to translate Dir).
func execRefusesDirEscape(t *testing.T, sb sandbox.Sandbox, _ string) {
	for _, dir := range []string{"../..", "../escape"} {
		_, err := sb.Exec(ctx(), sandbox.Command{Path: "sh", Args: []string{"-c", "pwd"}, Dir: dir})
		if err == nil {
			t.Errorf("Exec with escaping Dir %q was not refused", dir)
		}
	}
}

func capabilitiesAdvertised(t *testing.T, sb sandbox.Sandbox, _ string) {
	// Every backend must answer Capabilities() with a known isolation level — the
	// MinIsolation gate relies on it. We don't assert a SPECIFIC level (that's
	// backend-specific); we assert it's a recognized one.
	got := sb.Capabilities().Isolation
	switch got {
	case sandbox.IsolationNone, sandbox.IsolationProcess, sandbox.IsolationKernel, sandbox.IsolationVM:
	default:
		t.Errorf("Capabilities().Isolation = %v, not a recognized level", got)
	}
}
