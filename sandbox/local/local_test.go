package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

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
