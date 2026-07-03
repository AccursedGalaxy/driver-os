package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeAddSnapshotsMainDirtyTreeAndCollectedPatchAppliesBack(t *testing.T) {
	repo := newGitRepo(t)

	writeTestFile(t, filepath.Join(repo, "initial.txt"), "main dirty edit\n")
	writeTestFile(t, filepath.Join(repo, "untracked.txt"), "main-only\n")

	headSHA := strings.TrimSpace(string(git(t, repo, "rev-parse", "HEAD")))

	wi, err := WorktreeAdd(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	wt := wi.Dir
	defer func() { _ = WorktreeRemove(context.Background(), wt) }()

	if got := strings.TrimSpace(string(git(t, wt, "rev-parse", "--abbrev-ref", "HEAD"))); got != "HEAD" {
		t.Fatalf("worktree branch = %q, want detached HEAD", got)
	}
	if got := readTestFile(t, filepath.Join(wt, "initial.txt")); got != "main dirty edit\n" {
		t.Fatalf("tracked file = %q, want main checkout dirty contents", got)
	}
	if got := readTestFile(t, filepath.Join(wt, "untracked.txt")); got != "main-only\n" {
		t.Fatalf("untracked file = %q, want main checkout untracked contents", got)
	}

	// Baseline provenance: dirty tree → snapshot commit ≠ HEAD.
	if wi.BaseCommit == headSHA {
		t.Fatalf("BaseCommit = HEAD %s, want dirty-tree snapshot commit", headSHA)
	}
	refName := "refs/driver-agent/baselines/" + filepath.Base(wt)
	refSHA := strings.TrimSpace(string(git(t, repo, "rev-parse", refName)))
	if refSHA != wi.BaseCommit {
		t.Fatalf("ref %s resolves to %s, want %s (BaseCommit)", refName, refSHA, wi.BaseCommit)
	}
	// DirtyFiles must name the paths that differ from HEAD.
	foundInit := false
	foundUntracked := false
	for _, f := range wi.DirtyFiles {
		if f == "initial.txt" {
			foundInit = true
		}
		if f == "untracked.txt" {
			foundUntracked = true
		}
	}
	if !foundInit || !foundUntracked || len(wi.DirtyFiles) != 2 {
		t.Fatalf("DirtyFiles = %v, want [initial.txt, untracked.txt]", wi.DirtyFiles)
	}

	writeTestFile(t, filepath.Join(wt, "initial.txt"), "agent edit\n")
	patch := filepath.Join(t.TempDir(), "run.patch")
	changed, err := WorktreeCollect(context.Background(), wt, patch)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("WorktreeCollect changed=false, want true")
	}
	git(t, repo, "apply", "--", patch)
	if got := readTestFile(t, filepath.Join(repo, "initial.txt")); got != "agent edit\n" {
		t.Fatalf("applied patch initial.txt = %q, want agent edit", got)
	}
	if got := readTestFile(t, filepath.Join(repo, "untracked.txt")); got != "main-only\n" {
		t.Fatalf("applied patch disturbed untracked file: %q", got)
	}
}

func TestWorktreeCollectIncludesEditAndUntrackedFile(t *testing.T) {
	repo := newGitRepo(t)
	wi, err := WorktreeAdd(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	wt := wi.Dir
	defer func() { _ = WorktreeRemove(context.Background(), wt) }()

	writeTestFile(t, filepath.Join(wt, "initial.txt"), "after\n")
	writeTestFile(t, filepath.Join(wt, "new.txt"), "new file\n")
	patch := filepath.Join(t.TempDir(), "run.patch")
	changed, err := WorktreeCollect(context.Background(), wt, patch)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("WorktreeCollect changed=false, want true")
	}
	body := readTestFile(t, patch)
	for _, want := range []string{"initial.txt", "-initial", "+after", "new.txt", "+new file"} {
		if !strings.Contains(body, want) {
			t.Fatalf("patch missing %q:\n%s", want, body)
		}
	}
}

func TestWorktreeCollectCleanThenRemove(t *testing.T) {
	repo := newGitRepo(t)
	headSHA := strings.TrimSpace(string(git(t, repo, "rev-parse", "HEAD")))

	wi, err := WorktreeAdd(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	wt := wi.Dir

	// Clean checkout: BaseCommit must equal HEAD, no dirty files, no baseline ref.
	if wi.BaseCommit != headSHA {
		t.Fatalf("clean checkout BaseCommit = %s, want HEAD %s", wi.BaseCommit, headSHA)
	}
	if len(wi.DirtyFiles) != 0 {
		t.Fatalf("clean checkout DirtyFiles = %v, want empty", wi.DirtyFiles)
	}
	refName := "refs/driver-agent/baselines/" + filepath.Base(wt)
	if _, err := run(context.Background(), repo, "rev-parse", refName); err == nil {
		t.Fatalf("clean checkout created unexpected ref %s", refName)
	}

	patch := filepath.Join(t.TempDir(), "run.patch")
	changed, err := WorktreeCollect(context.Background(), wt, patch)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("clean worktree changed=true, want false")
	}
	if _, err := os.Stat(patch); !os.IsNotExist(err) {
		t.Fatalf("clean collect wrote patch: err=%v", err)
	}
	if err := WorktreeRemove(context.Background(), wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists after remove: err=%v", err)
	}
}

func TestWorktreeAddNonGitFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	_, err := WorktreeAdd(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("WorktreeAdd in a non-git dir succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not inside a git work tree") {
		t.Fatalf("error = %v, want clear non-git message", err)
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.email", "vcs-test@example.com")
	git(t, repo, "config", "user.name", "VCS Test")
	writeTestFile(t, filepath.Join(repo, "initial.txt"), "initial\n")
	git(t, repo, "add", "initial.txt")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

func git(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s failed: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return out
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
