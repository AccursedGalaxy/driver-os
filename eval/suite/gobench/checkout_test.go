package gobench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutBaseLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}

	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "src")
	cacheDir := filepath.Join(root, "cache")
	destDir := filepath.Join(root, "checkout")

	runGitTest(t, ctx, "", "init", repoDir)
	runGitTest(t, ctx, repoDir, "config", "user.name", "GoBench Test")
	runGitTest(t, ctx, repoDir, "config", "user.email", "gobench@example.test")

	want := "hello from base\n"
	if err := os.WriteFile(filepath.Join(repoDir, "hello.txt"), []byte(want), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runGitTest(t, ctx, repoDir, "add", "hello.txt")
	runGitTest(t, ctx, repoDir, "commit", "-m", "base")
	base := strings.TrimSpace(runGitTest(t, ctx, repoDir, "rev-parse", "HEAD"))

	if err := CheckoutBase(ctx, repoDir, base, destDir, cacheDir); err != nil {
		t.Fatalf("CheckoutBase: %v", err)
	}

	gotBytes, err := os.ReadFile(filepath.Join(destDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read checked-out file: %v", err)
	}
	if got := string(gotBytes); got != want {
		t.Fatalf("checked-out file = %q, want %q", got, want)
	}
	gotHead := strings.TrimSpace(runGitTest(t, ctx, destDir, "rev-parse", "HEAD"))
	if gotHead != base {
		t.Fatalf("checked-out HEAD = %s, want %s", gotHead, base)
	}
}

func runGitTest(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
