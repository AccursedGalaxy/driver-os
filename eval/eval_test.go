package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMapFixtureMaterialize(t *testing.T) {
	dir := t.TempDir()
	f := MapFixture{
		"go.mod":        "module x\n",
		"pkg/inner.txt": "hello",
	}
	if err := f.Materialize(dir); err != nil {
		t.Fatal(err)
	}
	// go.mod (a real go.mod, not a build problem here) lands verbatim at root.
	if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err != nil || string(b) != "module x\n" {
		t.Errorf("go.mod = %q, %v", b, err)
	}
	// A nested path created its parent directory.
	if b, err := os.ReadFile(filepath.Join(dir, "pkg", "inner.txt")); err != nil || string(b) != "hello" {
		t.Errorf("pkg/inner.txt = %q, %v", b, err)
	}
}

func TestMapFixtureDescribeIsSorted(t *testing.T) {
	f := MapFixture{"b.txt": "", "a.txt": "", "c.txt": ""}
	if got, want := f.Describe(), "a.txt, b.txt, c.txt"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}
