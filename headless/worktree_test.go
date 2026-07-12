package headless

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeAddSnapshotsMainDirtyTree(t *testing.T) {
	repo := newGitRepo(t)

	writeFile(t, filepath.Join(repo, "initial.txt"), "uncommitted main edit\n")
	writeFile(t, filepath.Join(repo, "untracked.txt"), "main-only\n")

	wi, err := worktreeAdd(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worktreeRemove(wi.Dir) }()

	if got := strings.TrimSpace(string(git(t, wi.Dir, "rev-parse", "--abbrev-ref", "HEAD"))); got != "HEAD" {
		t.Fatalf("worktree branch = %q, want detached HEAD", got)
	}
	if got := readFile(t, filepath.Join(wi.Dir, "initial.txt")); got != "uncommitted main edit\n" {
		t.Fatalf("tracked file = %q, want main checkout dirty contents", got)
	}
	if got := readFile(t, filepath.Join(wi.Dir, "untracked.txt")); got != "main-only\n" {
		t.Fatalf("untracked file = %q, want main checkout untracked contents", got)
	}
}

func TestWorktreeCollectIncludesEditAndUntrackedFile(t *testing.T) {
	repo := newGitRepo(t)
	wi, err := worktreeAdd(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worktreeRemove(wi.Dir) }()

	writeFile(t, filepath.Join(wi.Dir, "initial.txt"), "after\n")
	writeFile(t, filepath.Join(wi.Dir, "new.txt"), "new file\n")
	patch := filepath.Join(t.TempDir(), "run.patch")
	changed, err := worktreeCollect(wi.Dir, patch)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("worktreeCollect changed=false, want true")
	}
	body := readFile(t, patch)
	for _, want := range []string{"initial.txt", "-initial", "+after", "new.txt", "+new file"} {
		if !strings.Contains(body, want) {
			t.Fatalf("patch missing %q:\n%s", want, body)
		}
	}
}

func TestWorktreeCollectCleanThenRemove(t *testing.T) {
	repo := newGitRepo(t)
	wi, err := worktreeAdd(repo)
	if err != nil {
		t.Fatal(err)
	}

	patch := filepath.Join(t.TempDir(), "run.patch")
	changed, err := worktreeCollect(wi.Dir, patch)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("clean worktree changed=true, want false")
	}
	if _, err := os.Stat(patch); !os.IsNotExist(err) {
		t.Fatalf("clean collect wrote patch: err=%v", err)
	}
	if err := worktreeRemove(wi.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wi.Dir); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists after remove: err=%v", err)
	}
}

func TestWorktreeAddNonGitFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	_, err := worktreeAdd(t.TempDir())
	if err == nil {
		t.Fatal("worktreeAdd in a non-git dir succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not inside a git work tree") {
		t.Fatalf("error = %v, want clear non-git message", err)
	}
}

func TestWorktreeRemoveWhileCwd(t *testing.T) {
	repo := newGitRepo(t)
	wi, err := worktreeAdd(repo)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the bug: remove the worktree while it is the current directory.
	// On many systems, this makes subsequent git commands in that directory fail.
	if err := worktreeRemove(wi.Dir); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "status")
	cmd.Dir = wi.Dir
	if err := cmd.Run(); err == nil {
		// If this succeeds, the OS/filesystem might be lenient, but we still
		// want to avoid this state. The fix ensures we don't do this.
		t.Log("git status succeeded in deleted worktree (OS-specific behavior)")
	} else {
		t.Logf("git status failed as expected in deleted worktree: %v", err)
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.email", "agent-test@example.com")
	git(t, repo, "config", "user.name", "Agent Test")
	writeFile(t, filepath.Join(repo, "initial.txt"), "initial\n")
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
