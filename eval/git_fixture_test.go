package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitFixtureMaterialize checks the extraction half of GitFixture against
// this very repo at HEAD: the TRACKED tree is materialized, and no relative
// replace survives. (It must NOT assert that any particular dependency HAS a
// replace — aa367ce moved the mneme override from a go.mod replace to an
// untracked go.work, and the old layout-coupled assertion broke. The rewrite
// half is covered layout-independently by
// TestGitFixtureAbsolutizesRelativeReplace.)
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
}

// TestGitFixtureAbsolutizesRelativeReplace covers the rewrite half on a
// SYNTHETIC repo, so it cannot rot when this repo's own dependency layout
// changes: a committed go.mod with `replace … => ../sibling` must come out of
// Materialize pointing at the sibling's ABSOLUTE path (resolved against the
// original repo root, where the relative target meant something).
func TestGitFixtureAbsolutizesRelativeReplace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	parent := t.TempDir()
	sibling := filepath.Join(parent, "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "go.mod"), []byte("module example.com/sibling\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/repo\n\ngo 1.21\n\nrequire example.com/sibling v0.0.0\n\nreplace example.com/sibling => ../sibling\n"
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	dir := t.TempDir()
	if err := (GitFixture{RepoRoot: repo, Ref: "HEAD"}).Materialize(dir); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "=> ../") {
		t.Errorf("relative replace survived Materialize:\n%s", text)
	}
	if !strings.Contains(text, sibling) {
		t.Errorf("replace not absolutized to %q:\n%s", sibling, text)
	}
}
