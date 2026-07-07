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

func TestRestoreTreeRestoresModifiedDeletedAndAddedFiles(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	write(t, dir, "delete-me.txt", "keep me\n")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "nested/keep.txt", "nested original\n")
	base, err := WriteTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	write(t, dir, "README.md", "tampered\n")
	if err := os.Remove(filepath.Join(dir, "delete-me.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "added.txt", "new junk\n")
	write(t, dir, "nested/new.txt", "more junk\n")

	if err := RestoreTree(ctx, dir, base); err != nil {
		t.Fatal(err)
	}
	restored, err := WriteTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if restored != base {
		diff, _ := DiffTrees(ctx, dir, base, restored)
		t.Fatalf("restored tree = %s, want %s; diff:\n%s", restored, base, diff)
	}
	for name, want := range map[string]string{
		"README.md":       "hello\n",
		"delete-me.txt":   "keep me\n",
		"nested/keep.txt": "nested original\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q err=%v, want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"added.txt", "nested/new.txt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after RestoreTree (err=%v)", name, err)
		}
	}
	if out, _ := run(ctx, dir, "diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
		t.Fatalf("RestoreTree polluted the real index: %q", out)
	}
}

// RestoreTree must remove added files even when dir is a SUBDIRECTORY of the
// work tree (module_dir runs): git reports diff names relative to the repo
// toplevel, so joining them against dir silently skips every removal — the
// bug that made declare_repro reject genuinely new files on dolt-11215.
func TestRestoreTreeFromModuleSubdirRemovesAddedFiles(t *testing.T) {
	ctx := context.Background()
	root := initRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "go", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, "go/pkg/a.txt", "base\n")
	sub := filepath.Join(root, "go")
	base, err := WriteTree(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "go/pkg/new_test.go", "package pkg\n")
	write(t, root, "go/pkg/a.txt", "changed\n")
	if err := RestoreTree(ctx, sub, base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go", "pkg", "new_test.go")); !os.IsNotExist(err) {
		t.Fatalf("added file survived subdir RestoreTree (err=%v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "go", "pkg", "a.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("a.txt = %q err=%v, want restored to base", got, err)
	}
}

// RestoreTree must remove ignored artifacts left by a prior attempt, so each
// escalation rung starts from an exact baseline. WriteTree respects .gitignore,
// so a tree it restores never includes ignored files; without a clean step the
// cruft from attempt N survives into attempt N+1.
func TestRestoreTreeRemovesIgnoredArtifacts(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	// An ignore rule for a build-output directory, the kind of thing an agent
	// rung writes and the baseline must not inherit.
	write(t, dir, ".gitignore", "__pycache__/\n*.pyc\nbuild/\n")
	if err := StageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := Commit(ctx, dir, "add gitignore"); err != nil {
		t.Fatal(err)
	}
	// Baseline captured with NO ignored cruft present.
	base, err := WriteTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Attempt N writes ignored artifacts (a cache dir + a coverage file) plus a
	// real tracked edit, simulating what a rung leaves behind.
	if err := os.MkdirAll(filepath.Join(dir, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "__pycache__/foo.pyc", "compiled junk\n")
	write(t, dir, "build/out.txt", "build output\n")
	write(t, dir, "README.md", "tampered\n")

	// The reset for attempt N+1.
	if err := RestoreTree(ctx, dir, base); err != nil {
		t.Fatal(err)
	}

	// Ignored cruft from attempt N must be gone — the second attempt sees a
	// baseline with no leftover ignored artifact.
	for _, name := range []string{"__pycache__/foo.pyc", "build/out.txt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after RestoreTree (err=%v); ignored artifacts must not survive into the next attempt", name, err)
		}
	}
	// The tracked edit was restored to baseline.
	b, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || string(b) != "hello\n" {
		t.Errorf("README = %q (err %v), want baseline content restored", string(b), err)
	}
	// And the restored tree matches the baseline exactly (no ignored diff).
	restored, err := WriteTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if restored != base {
		diff, _ := DiffTrees(ctx, dir, base, restored)
		t.Fatalf("restored tree = %s, want %s; leftover diff:\n%s", restored, base, diff)
	}
}
