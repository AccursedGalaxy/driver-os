package gobench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

// FixtureError marks failures in GoBench fixture infrastructure (git mirrors,
// checkouts, cache storage) so callers can distinguish infra from task failure.
type FixtureError struct {
	Op   string
	Path string
	Err  error
}

func (e *FixtureError) Error() string {
	if e == nil {
		return "fixture error"
	}
	if e.Path != "" {
		return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *FixtureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func fixtureErr(op, path string, err error) error {
	if err == nil {
		return nil
	}
	return &FixtureError{Op: op, Path: path, Err: err}
}

// CheckoutBase materializes repoURL at baseCommit into destDir, using a local
// bare clone cache under cacheDir. The resulting destDir is a normal Git working
// tree suitable for git apply and for Grade's reset/clean operations.
func CheckoutBase(ctx context.Context, repoURL, baseCommit, destDir, cacheDir string) error {
	if repoURL == "" {
		return fixtureErr("validate checkout", "", fmt.Errorf("repoURL is empty"))
	}
	if baseCommit == "" {
		return fixtureErr("validate checkout", "", fmt.Errorf("baseCommit is empty"))
	}
	if destDir == "" {
		return fixtureErr("validate checkout", "", fmt.Errorf("destDir is empty"))
	}
	if cacheDir == "" {
		return fixtureErr("validate checkout", "", fmt.Errorf("cacheDir is empty"))
	}

	mirrorPath, err := EnsureBareMirror(ctx, repoURL, cacheDir)
	if err != nil {
		return err
	}

	if !gitHasCommit(ctx, mirrorPath, baseCommit) {
		if _, err := runGitCaptured(ctx, mirrorPath, "fetch", "origin", baseCommit); err != nil && !gitHasCommit(ctx, mirrorPath, baseCommit) {
			return fixtureErr("fetch base commit", mirrorPath, fmt.Errorf("base commit %s is not available in %s after direct fetch from %s: %w", baseCommit, mirrorPath, repoURL, err))
		}
	}
	if !gitHasCommit(ctx, mirrorPath, baseCommit) {
		return fixtureErr("fetch base commit", mirrorPath, fmt.Errorf("base commit %s is not available in %s after fetching from %s", baseCommit, mirrorPath, repoURL))
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fixtureErr("create checkout parent", destDir, fmt.Errorf("create parent for checkout %s: %w", destDir, err))
	}
	if _, err := runGitCaptured(ctx, "", "clone", mirrorPath, destDir); err != nil {
		return fixtureErr("clone worktree", destDir, fmt.Errorf("clone worktree from mirror %s to %s: %w", mirrorPath, destDir, err))
	}
	if _, err := runGitCaptured(ctx, destDir, "checkout", "--detach", baseCommit); err != nil {
		return fixtureErr("checkout base commit", destDir, fmt.Errorf("checkout base commit %s in %s: %w", baseCommit, destDir, err))
	}
	return nil
}

// EnsureBareMirror creates or refreshes the bare mirror used by GoBench git operations.
// Mirror creation is guarded by a per-mirror flock and is staged in a unique
// sibling temp dir before an atomic rename to the canonical path.
func EnsureBareMirror(ctx context.Context, repoURL, cacheDir string) (string, error) {
	if repoURL == "" {
		return "", fixtureErr("validate mirror", "", fmt.Errorf("repoURL is empty"))
	}
	if cacheDir == "" {
		return "", fixtureErr("validate mirror", "", fmt.Errorf("cacheDir is empty"))
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fixtureErr("create cache dir", cacheDir, fmt.Errorf("create cache dir %s: %w", cacheDir, err))
	}
	mirrorPath := filepath.Join(cacheDir, repoSlug(repoURL)+".git")
	unlock, err := lockMirror(mirrorPath + ".lock")
	if err != nil {
		return "", fixtureErr("lock mirror", mirrorPath, err)
	}
	defer unlock()

	if err := validateBareMirror(ctx, mirrorPath); err == nil {
		if _, err := runGitCaptured(ctx, mirrorPath, "fetch", "--all", "--prune"); err != nil {
			_ = os.RemoveAll(mirrorPath)
			return "", fixtureErr("refresh bare mirror", mirrorPath, fmt.Errorf("refresh bare mirror %s for %s: %w", mirrorPath, repoURL, err))
		}
		if err := validateBareMirror(ctx, mirrorPath); err != nil {
			_ = os.RemoveAll(mirrorPath)
			return "", fixtureErr("validate bare mirror", mirrorPath, err)
		}
		return mirrorPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(mirrorPath)
	}

	tmp, err := os.MkdirTemp(cacheDir, repoSlug(repoURL)+".tmp-*")
	if err != nil {
		return "", fixtureErr("create temp mirror", cacheDir, err)
	}
	if err := os.Remove(tmp); err != nil {
		return "", fixtureErr("prepare temp mirror", tmp, err)
	}
	defer os.RemoveAll(tmp)

	if _, err := runGitCaptured(ctx, "", "clone", "--bare", repoURL, tmp); err != nil {
		return "", fixtureErr("create bare mirror", mirrorPath, fmt.Errorf("create bare mirror for %s at %s: %w", repoURL, mirrorPath, err))
	}
	if err := validateBareMirror(ctx, tmp); err != nil {
		return "", fixtureErr("validate temp bare mirror", tmp, err)
	}
	if err := validateBareMirror(ctx, mirrorPath); err == nil {
		return mirrorPath, nil
	}
	_ = os.RemoveAll(mirrorPath)
	if err := os.Rename(tmp, mirrorPath); err != nil {
		if err := validateBareMirror(ctx, mirrorPath); err == nil {
			return mirrorPath, nil
		}
		_ = os.RemoveAll(mirrorPath)
		return "", fixtureErr("install bare mirror", mirrorPath, err)
	}
	if err := validateBareMirror(ctx, mirrorPath); err != nil {
		_ = os.RemoveAll(mirrorPath)
		return "", fixtureErr("validate bare mirror", mirrorPath, err)
	}
	return mirrorPath, nil
}

func lockMirror(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func validateBareMirror(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	// Cheap deterministic integrity check: rev-parse proves Git recognizes the
	// directory as the git-dir, and show-ref verifies the mirror has a readable
	// refs database. We avoid fs existence checks because partial clones can leave
	// plausible-looking directories without a valid object/ref store.
	if _, err := runGitCaptured(ctx, path, "rev-parse", "--git-dir"); err != nil {
		return err
	}
	if _, err := runGitCaptured(ctx, path, "show-ref", "--head"); err != nil {
		return err
	}
	return nil
}

func gitHasCommit(ctx context.Context, mirrorPath, commit string) bool {
	_, err := runGitCaptured(ctx, mirrorPath, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

type gitCommandError struct {
	args   []string
	dir    string
	stdout string
	stderr string
	err    error
}

func (e gitCommandError) Error() string {
	var b strings.Builder
	b.WriteString("git")
	for _, arg := range e.args {
		b.WriteByte(' ')
		b.WriteString(arg)
	}
	if e.dir != "" {
		b.WriteString(" in ")
		b.WriteString(e.dir)
	}
	b.WriteString(": ")
	b.WriteString(e.err.Error())
	out := strings.TrimSpace(e.stderr)
	if out == "" {
		out = strings.TrimSpace(e.stdout)
	}
	if out != "" {
		b.WriteString(": ")
		b.WriteString(out)
	}
	return b.String()
}
func (e gitCommandError) Unwrap() error { return e.err }

func runGitCaptured(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.String()
	if err != nil {
		return out, gitCommandError{args: args, dir: dir, stdout: out, stderr: stderr.String(), err: err}
	}
	return out, nil
}

func repoSlug(repoURL string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(repoURL) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 && b.String()[b.Len()-1] != '-' {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	if slug == "" {
		slug = "repo"
	}
	sum := sha256.Sum256([]byte(repoURL))
	return slug + "-" + hex.EncodeToString(sum[:])[:12]
}
