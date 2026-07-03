package vcs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorktreeAdd creates a throwaway detached worktree at exactly HEAD. It is used
// by front-ends before the sandbox is built, so every downstream effect sees the
// isolated checkout instead of the operator's main working tree.
func WorktreeAdd(ctx context.Context, origCwd string) (string, error) {
	inside, err := run(ctx, origCwd, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		if err != nil {
			return "", fmt.Errorf("cwd is not inside a git work tree: %w", err)
		}
		return "", fmt.Errorf("cwd is not inside a git work tree")
	}

	dir, err := os.MkdirTemp("", "driver-agent-wt-")
	if err != nil {
		return "", err
	}
	if _, err := run(ctx, origCwd, "worktree", "add", "--detach", dir, "HEAD"); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// WorktreeCollect stages untracked paths as intent-to-add, then writes a binary
// patch for all changes relative to HEAD. It returns changed=false when the
// worktree is clean and no patch was written. When patchPath is empty it only
// reports whether changes exist.
func WorktreeCollect(ctx context.Context, dir, patchPath string) (bool, error) {
	if _, err := run(ctx, dir, "add", "-A", "-N"); err != nil {
		return false, err
	}
	diff, err := run(ctx, dir, "diff", "--binary", "HEAD")
	if err != nil {
		return false, err
	}
	if len(diff) == 0 {
		return false, nil
	}
	if patchPath == "" {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(patchPath), 0o755); err != nil {
		return true, err
	}
	if err := os.WriteFile(patchPath, []byte(diff), 0o644); err != nil {
		return true, err
	}
	return true, nil
}

// WorktreeRemove removes a throwaway worktree and prunes stale worktree
// metadata.
func WorktreeRemove(ctx context.Context, dir string) error {
	common, err := run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	gitDir := strings.TrimSpace(common)
	if gitDir == "" {
		return fmt.Errorf("empty git common dir")
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	if _, err := run(ctx, "", "--git-dir", gitDir, "worktree", "remove", "--force", dir); err != nil {
		return err
	}
	if _, err := run(ctx, "", "--git-dir", gitDir, "worktree", "prune"); err != nil {
		return err
	}
	return nil
}
