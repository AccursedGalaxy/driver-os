package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goEnvDir is a small test helper: the canonical dir for `go env <name>`.
func goEnvDir(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", name, err)
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

func TestGoSourcePathBoundary(t *testing.T) {
	goroot := goEnvDir(t, "GOROOT")
	modcache := goEnvDir(t, "GOMODCACHE")

	inside := []string{
		filepath.Join(goroot, "src", "strings", "builder.go"),
		filepath.Join(modcache, "github.com", "x", "y@v1.0.0", "z.go"),
		goroot, // the root itself
	}
	for _, p := range inside {
		if _, ok := goSourcePath(p); !ok {
			t.Errorf("goSourcePath(%q) = false, want true (inside a Go source tree)", p)
		}
	}

	outside := []string{
		"strings/builder.go",                     // relative — belongs to the workspace fence
		"/etc/passwd",                            // absolute, not a Go tree
		filepath.Join(goroot, "..", "..", "etc"), // traversal that cleans ABOVE the root
		filepath.Join(modcache, "..", "secret"),  // ditto for the module cache
	}
	for _, p := range outside {
		if abs, ok := goSourcePath(p); ok {
			t.Errorf("goSourcePath(%q) = %q,true, want false (must stay fenced)", p, abs)
		}
	}
}

// TestReadFileReadsStdlibSource proves read_file serves an absolute GOROOT path
// host-side, with line numbers, including a line range.
func TestReadFileReadsStdlibSource(t *testing.T) {
	sb := sbWith(t, nil) // an empty workspace — the source is NOT inside it.
	builder := filepath.Join(goEnvDir(t, "GOROOT"), "src", "strings", "builder.go")

	out, err := readFileOp(context.Background(), sb, builder, 1, 5, true, ReadOptions{})
	if err != nil {
		t.Fatalf("readFileOp(%q): %v", builder, err)
	}
	if !strings.Contains(out, "package strings") {
		t.Errorf("read_file on stdlib source missing `package strings`:\n%s", out)
	}
	if !strings.Contains(out, "1| ") {
		t.Errorf("read_file output is not line-numbered:\n%s", out)
	}
}

// TestListDirListsStdlibSource proves list_dir serves an absolute GOROOT dir.
func TestListDirListsStdlibSource(t *testing.T) {
	sb := sbWith(t, nil)
	dir := filepath.Join(goEnvDir(t, "GOROOT"), "src", "strings")
	out, err := listDirOp(context.Background(), sb, dir)
	if err != nil {
		t.Fatalf("listDirOp(%q): %v", dir, err)
	}
	if !strings.Contains(out, "builder.go") {
		t.Errorf("list_dir on stdlib dir missing builder.go:\n%s", out)
	}
}

// TestWriteStaysFenced confirms the read-only widening did NOT leak to writes: an
// absolute GOROOT path must still be refused by the write op (the fence).
func TestWriteStaysFenced(t *testing.T) {
	sb := sbWith(t, nil)
	target := filepath.Join(goEnvDir(t, "GOROOT"), "src", "strings", "pwned.go")
	if _, err := writeFileOp(context.Background(), sb, target, "package strings", false); err == nil {
		t.Fatalf("writeFileOp(%q) succeeded — an absolute path outside the workspace must be refused", target)
	}
}

// TestSearchSearchesStdlibSource proves search greps an absolute GOROOT dir
// host-side and returns citable path:line:text.
func TestSearchSearchesStdlibSource(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	sb := sbWith(t, nil)
	dir := filepath.Join(goEnvDir(t, "GOROOT"), "src", "strings")
	out, err := searchOp(context.Background(), sb, "copyCheck", dir, 10*time.Second)
	if err != nil {
		t.Fatalf("searchOp: %v", err)
	}
	if !strings.Contains(out, "builder.go") {
		t.Errorf("search of stdlib dir did not find the match in builder.go:\n%s", out)
	}
}
