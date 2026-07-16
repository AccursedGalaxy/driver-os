package vcs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// WorktreeInfo is the provenance record for a throwaway worktree created by
// WorktreeAdd. Dir is the detached worktree path. BaseCommit is the full SHA
// the worktree is based on: HEAD when the main checkout was clean, the
// dirty-tree snapshot commit when it was dirty. DirtyFiles names the paths
// that differ between HEAD and the snapshot (sorted), nil when clean.
type WorktreeInfo struct {
	Dir        string
	BaseCommit string
	DirtyFiles []string
}

// WorktreeAdd creates a throwaway detached worktree. Clean checkouts are based
// exactly on HEAD. Dirty checkouts are first snapshotted into a commit pinned
// under refs/driver-agent/baselines/<name> that includes tracked modifications
// and untracked, non-ignored files, so delegated work sees the orchestrator's
// current tree while patches still diff cleanly against the worktree's own HEAD.
func WorktreeAdd(ctx context.Context, origCwd string) (WorktreeInfo, error) {
	inside, err := run(ctx, origCwd, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		if err != nil {
			return WorktreeInfo{}, fmt.Errorf("cwd is not inside a git work tree: %w", err)
		}
		return WorktreeInfo{}, fmt.Errorf("cwd is not inside a git work tree")
	}

	headSHA, err := run(ctx, origCwd, "rev-parse", "HEAD")
	if err != nil {
		return WorktreeInfo{}, err
	}
	headSHA = strings.TrimSpace(headSHA)

	base := "HEAD"
	baseCommit := headSHA
	var dirtyFiles []string

	headTree, err := run(ctx, origCwd, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return WorktreeInfo{}, err
	}
	workTree, err := WriteTree(ctx, origCwd)
	if err != nil {
		return WorktreeInfo{}, err
	}
	if workTree != strings.TrimSpace(headTree) {
		env := []string{
			"GIT_AUTHOR_NAME=driver-agent",
			"GIT_AUTHOR_EMAIL=driver-agent@localhost",
			"GIT_COMMITTER_NAME=driver-agent",
			"GIT_COMMITTER_EMAIL=driver-agent@localhost",
		}
		commit, err := runEnv(ctx, origCwd, env, "commit-tree", workTree, "-p", "HEAD", "-m", "driver-agent: dirty-tree snapshot")
		if err != nil {
			return WorktreeInfo{}, err
		}
		base = strings.TrimSpace(commit)
		baseCommit = base
	}

	dir, err := os.MkdirTemp("", "driver-agent-wt-")
	if err != nil {
		return WorktreeInfo{}, err
	}
	if _, err := run(ctx, origCwd, "worktree", "add", "--detach", dir, base); err != nil {
		_ = os.RemoveAll(dir)
		return WorktreeInfo{}, err
	}

	if err := rewriteRelativeReplaceTargets(ctx, origCwd, dir); err != nil {
		_ = WorktreeRemove(ctx, dir)
		return WorktreeInfo{}, err
	}

	// Dirty checkouts: pin the snapshot commit under a ref so it survives
	// git gc, and enumerate the paths that differ from HEAD.
	if baseCommit != headSHA {
		refName := "refs/driver-agent/baselines/" + filepath.Base(dir)
		if _, err := run(ctx, origCwd, "update-ref", refName, baseCommit); err != nil {
			// The worktree is already created; don't fail the whole call
			// over a ref write, but do report the error so the caller can
			// decide.
			return WorktreeInfo{Dir: dir, BaseCommit: baseCommit}, fmt.Errorf("pinning baseline ref %s: %w", refName, err)
		}
		names, err := diffTreeNames(ctx, origCwd, headSHA, baseCommit)
		if err != nil {
			return WorktreeInfo{Dir: dir, BaseCommit: baseCommit}, fmt.Errorf("enumerating dirty files: %w", err)
		}
		sort.Strings(names)
		dirtyFiles = names
	}

	return WorktreeInfo{Dir: dir, BaseCommit: baseCommit, DirtyFiles: dirtyFiles}, nil
}

// rewriteRelativeReplaceTargets makes root go.mod replacements usable from a
// throwaway worktree outside the original checkout. The setup change is committed
// on the detached worktree HEAD, so WorktreeCollect never includes it in an agent
// patch. It intentionally leaves go.work alone.
func rewriteRelativeReplaceTargets(ctx context.Context, origCwd, worktreeDir string) error {
	worktreeMod := filepath.Join(worktreeDir, "go.mod")
	data, err := os.ReadFile(worktreeMod)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read worktree go.mod: %w", err)
	}

	mod, err := modfile.Parse(worktreeMod, data, nil)
	if err != nil {
		return fmt.Errorf("parse worktree go.mod: %w", err)
	}
	var relative []*modfile.Replace
	for _, replace := range mod.Replace {
		if replace.New.Version == "" && isRelativeReplacePath(replace.New.Path) {
			relative = append(relative, replace)
		}
	}
	if len(relative) == 0 {
		return nil
	}

	root, err := run(ctx, origCwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("find original checkout root: %w", err)
	}
	root = strings.TrimSpace(root)
	for _, replace := range relative {
		target := filepath.Clean(filepath.Join(root, replace.New.Path))
		if err := mod.AddReplace(replace.Old.Path, replace.Old.Version, target, ""); err != nil {
			return fmt.Errorf("rewrite replace %s: %w", replace.Old.Path, err)
		}
	}
	if err := os.WriteFile(worktreeMod, modfile.Format(mod.Syntax), 0o644); err != nil {
		return fmt.Errorf("write worktree go.mod: %w", err)
	}

	env := []string{
		"GIT_AUTHOR_NAME=driver-agent",
		"GIT_AUTHOR_EMAIL=driver-agent@localhost",
		"GIT_COMMITTER_NAME=driver-agent",
		"GIT_COMMITTER_EMAIL=driver-agent@localhost",
	}
	if _, err := run(ctx, worktreeDir, "add", "go.mod"); err != nil {
		return fmt.Errorf("stage worktree go.mod rewrite: %w", err)
	}
	if _, err := runEnv(ctx, worktreeDir, env, "-c", "commit.gpgSign=false", "commit", "--no-verify", "-m", "driver-agent: resolve relative go.mod replacements"); err != nil {
		return fmt.Errorf("commit worktree go.mod rewrite: %w", err)
	}
	return nil
}

func isRelativeReplacePath(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../")
}

// diffTreeNames returns the sorted paths that differ between two tree-ish objects
// using git diff-tree. NUL-separated for safety with arbitrary filenames.
func diffTreeNames(ctx context.Context, dir, a, b string) ([]string, error) {
	out, err := run(ctx, dir, "diff-tree", "-r", "--name-only", "-z", a, b)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// WorktreeCollect stages untracked paths as intent-to-add, then writes a binary
// patch for all changes relative to HEAD. It returns changed=false when the
// worktree is clean and no patch was written. When patchPath is empty it only
// reports whether changes exist.
func WorktreeCollect(ctx context.Context, dir, patchPath string) (bool, error) {
	if _, err := os.Stat(dir); err != nil {
		return false, err
	}
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
