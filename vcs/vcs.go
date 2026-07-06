// Package vcs is the git surface for the agent's diff-review flow: the host-side
// helpers that let a human SEE what the agent changed and decide whether to keep
// it, before anything is committed. It is the other half of the trust layer
// (sandbox/gated gates what the agent RUNS; this reviews what it WROTE).
//
// It shells out to the `git` binary rather than linking a git library on
// purpose: these run on the host against the user's real repo (not inside the
// sandbox), the operations are a handful of porcelain commands, and matching the
// user's own git exactly (config, hooks, version) is a feature, not a thing to
// reimplement. Every call is `git -C <dir> …` so the working directory is
// explicit and nothing depends on the process's cwd.
package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsClean reports whether dir is a git work tree with NO uncommitted changes
// (tracked or untracked). The -review flow requires this up front: a clean tree
// means the post-run diff is unambiguously the agent's work, and discarding is
// safe because nothing of the user's is mixed in. A dir that isn't a git repo
// returns an error, not false — "not a repo" and "dirty repo" need different
// messages to the user.
func IsClean(ctx context.Context, dir string) (bool, error) {
	if _, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return false, fmt.Errorf("%s is not a git work tree: %w", dir, err)
	}
	out, err := run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// StageAll stages every change — modified, deleted, and NEW files — so the
// review diff and a subsequent commit include the agent's brand-new files (a
// plain `git diff` would miss them). It is the setup step for Diff/Commit.
func StageAll(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "add", "-A")
	return err
}

// Diff returns the staged diff (`git diff --cached`) — the full, reviewable set
// of the agent's changes after StageAll, including new files. Empty output means
// the agent changed nothing.
func Diff(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "diff", "--cached")
}

// Commit records the staged changes with msg. It assumes StageAll already ran
// (the -review flow stages, shows the diff, then commits on approval).
func Commit(ctx context.Context, dir, msg string) error {
	_, err := run(ctx, dir, "commit", "-m", msg)
	return err
}

// Discard throws the agent's work away and returns the tree to HEAD: it unstages
// + reverts tracked changes (reset --hard) and removes new untracked files
// (clean -fd). Safe precisely because IsClean was required first — there is no
// pre-existing uncommitted work of the user's to lose.
func Discard(ctx context.Context, dir string) error {
	if _, err := run(ctx, dir, "reset", "--hard", "HEAD"); err != nil {
		return err
	}
	_, err := run(ctx, dir, "clean", "-fd")
	return err
}

// Unstage moves the staged changes back to the working tree (`git reset`),
// leaving the files on disk for the human to inspect or edit by hand. It is the
// "keep, but don't commit" outcome of the review.
func Unstage(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "reset")
	return err
}

// IsRepo reports whether dir is inside a git work tree. Unlike IsClean it does
// not care about dirtiness — the review gate diffs against a recorded start
// state, so a dirty tree is fine; only "no repo at all" disables it.
func IsRepo(ctx context.Context, dir string) bool {
	_, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// WriteTree snapshots the ENTIRE working tree — tracked and untracked files,
// gitignore respected — as a git tree object and returns its hash. It stages
// into a TEMPORARY index (GIT_INDEX_FILE), so the repository's real index,
// HEAD, and the user's staged state are untouched; the only side effect is
// loose blobs in the object store, which gc reclaims. This is the review
// gate's diff baseline: two WriteTree calls bracket the agent's work and
// DiffTrees renders exactly what changed between them, new files included.
func WriteTree(ctx context.Context, dir string) (string, error) {
	// The index path must NOT pre-exist: git treats an existing zero-length
	// index as corrupt ("index file smaller than expected"), so hand it a fresh
	// name inside a temp dir rather than a pre-created empty file.
	tmpDir, err := os.MkdirTemp("", "driver-os-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	env := []string{"GIT_INDEX_FILE=" + tmpDir + "/index"}
	if _, err := runEnv(ctx, dir, env, "add", "-A"); err != nil {
		return "", err
	}
	out, err := runEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// WriteTreeCached is WriteTree's warm-index variant. It stages into the caller's
// persistent indexPath (never the repository index), so repeated calls can reuse
// git's stat cache while preserving WriteTree's tracked+untracked, gitignore-aware
// semantics. The index path's parent is created if needed.
func WriteTreeCached(ctx context.Context, dir, indexPath string) (string, error) {
	if strings.TrimSpace(indexPath) == "" {
		return WriteTree(ctx, dir)
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := runEnv(ctx, dir, env, "add", "-A"); err != nil {
		return "", err
	}
	out, err := runEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// DiffTrees returns the unified diff between two tree objects (as produced by
// WriteTree). Empty output means the trees are identical.
func DiffTrees(ctx context.Context, dir, a, b string) (string, error) {
	return run(ctx, dir, "diff", a, b)
}

// DiffTreesStat returns git's short stat summary between two tree objects.
func DiffTreesStat(ctx context.Context, dir, a, b string) (string, error) {
	return run(ctx, dir, "diff", "--stat", "--shortstat", a, b)
}

// DiffTreeNames returns the paths whose content differs between two tree objects.
func DiffTreeNames(ctx context.Context, dir, a, b string) ([]string, error) {
	out, err := run(ctx, dir, "diff", "--name-only", "-z", a, b)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// RestoreTree restores the working tree to the exact content captured in tree,
// where tree is a hash returned by WriteTree. It uses a temporary index for all
// git plumbing, so the user's real index is not read or modified. Files present
// in the current working tree but absent from tree are removed; modified and
// deleted files are restored from tree.
func RestoreTree(ctx context.Context, dir, tree string) error {
	cur, err := WriteTree(ctx, dir)
	if err != nil {
		return err
	}
	added, err := diffTreeNamesFiltered(ctx, dir, tree, cur, "A")
	if err != nil {
		return err
	}
	for _, name := range added {
		path, ok := safeTreePath(dir, name)
		if !ok {
			return fmt.Errorf("refusing to remove unsafe git path %q", name)
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		pruneEmptyParents(dir, name)
	}

	// The index path must not pre-exist; see WriteTree for the same git quirk.
	tmpDir, err := os.MkdirTemp("", "driver-os-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	env := []string{"GIT_INDEX_FILE=" + tmpDir + "/index"}
	if _, err := runEnv(ctx, dir, env, "read-tree", tree); err != nil {
		return err
	}
	if _, err := runEnv(ctx, dir, env, "checkout-index", "-af"); err != nil {
		return err
	}
	// WriteTree respects .gitignore, so a restored tree never contains
	// ignored files (__pycache__, build dirs, coverage output). Without this
	// step, ignored artifacts written by one escalation attempt survive into
	// the next, breaking attempt isolation. `clean -fdX` removes ONLY ignored
	// files and directories — tracked and untracked-non-ignored state was
	// already reconciled above — so each rung starts from an exact baseline.
	_, err = run(ctx, dir, "clean", "-fdX")
	return err
}

func diffTreeNamesFiltered(ctx context.Context, dir, a, b, filter string) ([]string, error) {
	out, err := run(ctx, dir, "diff", "--name-only", "-z", "--diff-filter="+filter, a, b)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

func splitNUL(out string) []string {
	if out == "" {
		return nil
	}
	raw := strings.Split(out, "\x00")
	paths := raw[:0]
	for _, p := range raw {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func safeTreePath(root, name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.Join(root, clean), true
}

func pruneEmptyParents(root, name string) {
	parent := filepath.Dir(filepath.Clean(filepath.FromSlash(name)))
	for parent != "." && parent != string(os.PathSeparator) {
		if err := os.Remove(filepath.Join(root, parent)); err != nil {
			return
		}
		parent = filepath.Dir(parent)
	}
}

// Apply applies a patch file to the working tree (`git apply`). Used by the
// gate-only evaluation harness to install a banked candidate diff.
func Apply(ctx context.Context, dir, patchPath string) error {
	_, err := run(ctx, dir, "apply", "--", patchPath)
	return err
}

// run executes `git -C dir args…` and returns stdout, folding stderr into the
// error so a failure message is actionable.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, args...)
}

// runEnv is run with extra environment entries (e.g. a temporary
// GIT_INDEX_FILE) merged over the inherited environment.
func runEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
