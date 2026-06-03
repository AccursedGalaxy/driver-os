package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitFixtureMaterialize checks the two things GitFixture does that a plain
// file copy does not: it extracts the TRACKED tree at a ref, and it rewrites a
// relative replace directive to an absolute path so the local-dep build survives
// the move out of the live repo. It runs against this very repo at HEAD.
func TestGitFixtureMaterialize(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if out, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not a git work tree")
	}

	dir := t.TempDir()
	if err := (GitFixture{RepoRoot: root, Ref: "HEAD"}).Materialize(dir); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// The tracked tree was extracted: a long-committed file is present. (git
	// archive emits only COMMITTED files, so this asserts on eval.go, not on a
	// freshly-added uncommitted source file.)
	if _, err := os.Stat(filepath.Join(dir, "eval", "eval.go")); err != nil {
		t.Errorf("expected eval/eval.go in the materialized tree: %v", err)
	}

	// The relative `replace => ../mneme` was absolutized: the go.mod no longer
	// carries a relative target, and the rewritten one points at a real abs path.
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	text := string(gomod)
	if strings.Contains(text, "=> ../") || strings.Contains(text, "=> ./") {
		t.Errorf("go.mod still has a relative replace after Materialize:\n%s", text)
	}
	if strings.Contains(text, "AccursedGalaxy/mneme") {
		// the mneme replace must now be absolute and point at an existing dir.
		abs := filepath.Clean(filepath.Join(root, "..", "mneme"))
		if !strings.Contains(text, abs) {
			t.Errorf("mneme replace not absolutized to %q:\n%s", abs, text)
		}
	}
}
