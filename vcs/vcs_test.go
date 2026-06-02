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
