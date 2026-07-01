package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo makes a fresh git repo in a temp dir with one committed file, so each
// test starts from a clean, known HEAD.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := run(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := Commit(ctx, dir, "init"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsCleanDetectsDirty(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)

	if clean, err := IsClean(ctx, dir); err != nil || !clean {
		t.Fatalf("fresh repo: clean=%v err=%v, want clean", clean, err)
	}
	write(t, dir, "new.txt", "x") // an untracked file makes it dirty
	if clean, err := IsClean(ctx, dir); err != nil || clean {
		t.Fatalf("after adding an untracked file: clean=%v err=%v, want dirty", clean, err)
	}
}

func TestIsCleanRejectsNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	if _, err := IsClean(context.Background(), t.TempDir()); err == nil {
		t.Error("a non-repo dir should error, not report clean/dirty")
	}
}

func TestStageDiffIncludesNewFiles(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	write(t, dir, "README.md", "hello\nworld\n") // modify tracked
	write(t, dir, "added.go", "package x\n")     // brand-new file

	if err := StageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	diff, err := Diff(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "added.go") {
		t.Errorf("diff should include the new file; got:\n%s", diff)
	}
	if !strings.Contains(diff, "world") {
		t.Errorf("diff should include the tracked modification; got:\n%s", diff)
	}
}

func TestCommitClearsTree(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	write(t, dir, "added.go", "package x\n")
	if err := StageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := Commit(ctx, dir, "add file"); err != nil {
		t.Fatal(err)
	}
	if clean, err := IsClean(ctx, dir); err != nil || !clean {
		t.Fatalf("after commit: clean=%v err=%v, want clean", clean, err)
	}
}

func TestDiscardRestoresHead(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	write(t, dir, "README.md", "tampered\n")
	write(t, dir, "junk.txt", "remove me")
	if err := StageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := Discard(ctx, dir); err != nil {
		t.Fatal(err)
	}
	// Tracked file is back to HEAD, untracked file is gone, tree is clean.
	b, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || string(b) != "hello\n" {
		t.Errorf("README = %q (err %v), want the HEAD content restored", string(b), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Error("Discard should remove untracked files")
	}
	if clean, _ := IsClean(ctx, dir); !clean {
		t.Error("Discard should leave a clean tree")
	}
}

func TestUnstageKeepsChangesInWorkingTree(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	write(t, dir, "added.go", "package x\n")
	if err := StageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := Unstage(ctx, dir); err != nil {
		t.Fatal(err)
	}
	// The file is still on disk (kept), just no longer staged.
	if _, err := os.Stat(filepath.Join(dir, "added.go")); err != nil {
		t.Errorf("Unstage must keep the file on disk: %v", err)
	}
	if clean, _ := IsClean(ctx, dir); clean {
		t.Error("Unstage keeps changes, so the tree is still dirty")
	}
}

// WriteTree snapshots the whole working tree (untracked files included)
// without touching the repo's real index; two snapshots bracket a change and
// DiffTrees renders exactly that change.
func TestWriteTreeDiffTreesBracketsChange(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	write(t, dir, "untracked.txt", "present from the start\n")
	base, err := WriteTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// The temp-index snapshot must not stage anything for real.
	if out, _ := run(ctx, dir, "diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
		t.Fatalf("WriteTree polluted the real index: %q", out)
	}
	// No change → identical trees, empty diff.
	same, err := WriteTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if same != base {
		t.Fatalf("unchanged tree hashed differently: %s vs %s", base, same)
	}
	// A tracked edit AND a brand-new file both appear in the bracketed diff.
	write(t, dir, "README.md", "hello\nworld\n")
	write(t, dir, "new.go", "package new\n")
	cur, err := WriteTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := DiffTrees(ctx, dir, base, cur)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+world", "new.go", "+package new"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "untracked.txt") {
		t.Error("unchanged untracked file must not appear in the bracketed diff")
	}
}

// WriteTree works in a repo with NO commits — the gate-only workspaces are
// sometimes bare-init'd — and IsRepo distinguishes repo-ness from cleanness.
func TestWriteTreeOnCommitlessRepoAndIsRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := run(ctx, dir, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "x\n")
	if _, err := WriteTree(ctx, dir); err != nil {
		t.Fatalf("WriteTree on a commitless repo: %v", err)
	}
	if !IsRepo(ctx, dir) {
		t.Error("IsRepo must be true for a fresh init")
	}
	if IsRepo(ctx, t.TempDir()) {
		t.Error("IsRepo must be false outside a repo")
	}
}
