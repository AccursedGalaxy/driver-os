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
	"os/exec"
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

// run executes `git -C dir args…` and returns stdout, folding stderr into the
// error so a failure message is actionable.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
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
