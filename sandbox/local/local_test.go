package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/sandboxtest"
)

// TestConformance runs the shared backend contract suite against the local
// backend. It runs ALWAYS (no daemon needed) and is the parity baseline the
// docker backend is held to under the docker_integration tag.
func TestConformance(t *testing.T) {
	sandboxtest.RunConformance(t, func(t *testing.T, dir string) (sandbox.Sandbox, func()) {
		sb, err := New(dir)
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		return sb, func() { _ = sb.Close() }
	})
}

// TestSessionConformance runs the shared Session contract against the local
// backend (always, no daemon). It is the parity baseline for the docker session.
func TestSessionConformance(t *testing.T) {
	sandboxtest.RunSessionConformance(t, func(t *testing.T, dir string) (sandbox.Sessioner, func()) {
		sb, err := New(dir)
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		return sb, func() { _ = sb.Close() }
	})
}

// TestProcessConformance runs the shared ProcessHost contract against the local
// backend (always, no daemon) — the parity baseline for the docker process host.
func TestProcessConformance(t *testing.T) {
	sandboxtest.RunProcessConformance(t, func(t *testing.T, dir string) (sandbox.ProcessHost, func()) {
		sb, err := New(dir)
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		return sb, func() { _ = sb.Close() }
	})
}

func newTest(t *testing.T) (*Sandbox, string) {
	t.Helper()
	root := t.TempDir()
	sb, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sb, root
}

func TestCapabilitiesIsNone(t *testing.T) {
	sb, _ := newTest(t)
	if got := sb.Capabilities().Isolation; got != sandbox.IsolationNone {
		t.Errorf("isolation = %v, want none", got)
	}
}

func TestReadAndList(t *testing.T) {
	sb, root := newTest(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := sb.ReadFile(context.Background(), "hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hi" {
		t.Errorf("read = %q, want hi", data)
	}
	entries, err := sb.ListDir(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "hello.txt" || entries[0].IsDir {
		t.Errorf("entries = %+v, want one file hello.txt", entries)
	}
}

func TestAppendFile(t *testing.T) {
	sb, root := newTest(t)
	// A missing file is created (`>>` semantics).
	if err := sb.AppendFile(context.Background(), "log.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatalf("AppendFile create: %v", err)
	}
	// Appending lands at the end, replacing nothing.
	if err := sb.AppendFile(context.Background(), "log.txt", []byte("two\n"), 0o644); err != nil {
		t.Fatalf("AppendFile append: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("file = %q, want \"one\\ntwo\\n\"", data)
	}
	// The fence still applies.
	if err := sb.AppendFile(context.Background(), "../escape.txt", []byte("x"), 0o644); err == nil {
		t.Error("AppendFile escaped the fence")
	}
}

func TestReadFileLimit(t *testing.T) {
	sb, root := newTest(t)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Under the limit: full content, not truncated.
	data, trunc, err := sb.ReadFileLimit(context.Background(), "big.txt", 100)
	if err != nil || trunc || string(data) != "0123456789" {
		t.Errorf("under limit: data=%q trunc=%v err=%v", data, trunc, err)
	}
	// Over the limit: exactly the first max bytes, truncated=true.
	data, trunc, err = sb.ReadFileLimit(context.Background(), "big.txt", 4)
	if err != nil || !trunc || string(data) != "0123" {
		t.Errorf("over limit: data=%q trunc=%v err=%v, want \"0123\" trunc=true", data, trunc, err)
	}
	// Limit exactly at file size is NOT truncation.
	if _, trunc, _ := sb.ReadFileLimit(context.Background(), "big.txt", 10); trunc {
		t.Errorf("limit == size reported truncated; want false")
	}
	// The fence still applies to the bounded path.
	if _, _, err := sb.ReadFileLimit(context.Background(), "../escape.txt", 10); err == nil {
		t.Errorf("ReadFileLimit escaped the fence")
	}
}

func TestWriteRoundTrip(t *testing.T) {
	sb, _ := newTest(t)
	if err := sb.WriteFile(context.Background(), "data.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := sb.ReadFile(context.Background(), "data.txt")
	if err != nil || string(data) != "x" {
		t.Errorf("roundtrip = %q, %v", data, err)
	}
}

func TestFenceRefusesEscape(t *testing.T) {
	sb, _ := newTest(t)
	for _, p := range []string{"../escape.txt", "../../etc/passwd"} {
		if _, err := sb.ReadFile(context.Background(), p); err == nil {
			t.Errorf("ReadFile(%q) = nil error, want refusal", p)
		}
	}
}

func TestMountAliasTranslatesToRoot(t *testing.T) {
	// DUET-DOGFOOD F4: a model living in a container at the mount point uses
	// absolute paths like "/workspace/f.txt"; with the alias set those must name
	// the same files as root-relative paths — for reads, writes, and listing the
	// mount point itself.
	sb, root := newTest(t)
	sb.SetMountAlias("/workspace")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := sb.ReadFile(context.Background(), "/workspace/f.txt")
	if err != nil || string(data) != "hi" {
		t.Errorf("ReadFile(/workspace/f.txt) = %q, %v; want hi", data, err)
	}
	if err := sb.WriteFile(context.Background(), "/workspace/g.txt", []byte("x"), 0o644); err != nil {
		t.Errorf("WriteFile(/workspace/g.txt): %v", err)
	}
	if data, err := sb.ReadFile(context.Background(), "g.txt"); err != nil || string(data) != "x" {
		t.Errorf("aliased write not visible root-relative: %q, %v", data, err)
	}
	if _, err := sb.ListDir(context.Background(), "/workspace"); err != nil {
		t.Errorf("ListDir(/workspace): %v", err)
	}
}

func TestMountAliasStillFences(t *testing.T) {
	// The alias is a translation, not a widening: an escape THROUGH the alias and
	// any unaliased absolute path stay refused, and without an alias the
	// mount-point path is refused as before.
	sb, _ := newTest(t)
	sb.SetMountAlias("/workspace")
	for _, p := range []string{"/workspace/../escape.txt", "/etc/passwd", "/workspaceX/f.txt"} {
		if _, err := sb.ReadFile(context.Background(), p); err == nil {
			t.Errorf("ReadFile(%q) = nil error, want refusal", p)
		}
	}
	plain, _ := newTest(t)
	if _, err := plain.ReadFile(context.Background(), "/workspace/f.txt"); err == nil {
		t.Errorf("no alias set: ReadFile(/workspace/f.txt) = nil error, want refusal")
	}
}

func TestExecEcho(t *testing.T) {
	sb, _ := newTest(t)
	res, err := sb.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "hi" {
		t.Errorf("stdout = %q, want hi", got)
	}
}

func TestExecNonZeroIsNotError(t *testing.T) {
	sb, _ := newTest(t)
	res, err := sb.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatalf("Exec returned Go error for non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
}

func TestExecTimeout(t *testing.T) {
	sb, _ := newTest(t)
	res, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "sleep 5"}, Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true")
	}
}

func TestExecWorkingDir(t *testing.T) {
	sb, root := newTest(t)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "pwd"}, Dir: "sub"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "sub"))
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(res.Stdout)))
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}
