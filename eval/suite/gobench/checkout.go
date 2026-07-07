package gobench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// CheckoutBase materializes repoURL at baseCommit into destDir, using a local
// bare clone cache under cacheDir. The resulting destDir is a normal Git working
// tree suitable for git apply and for Grade's reset/clean operations.
func CheckoutBase(ctx context.Context, repoURL, baseCommit, destDir, cacheDir string) error {
	if repoURL == "" {
		return fmt.Errorf("repoURL is empty")
	}
	if baseCommit == "" {
		return fmt.Errorf("baseCommit is empty")
	}
	if destDir == "" {
		return fmt.Errorf("destDir is empty")
	}
	if cacheDir == "" {
		return fmt.Errorf("cacheDir is empty")
	}

	mirrorPath, err := EnsureBareMirror(ctx, repoURL, cacheDir)
	if err != nil {
		return err
	}

	if !gitHasCommit(ctx, mirrorPath, baseCommit) {
		if _, err := runGitCaptured(ctx, mirrorPath, "fetch", "origin", baseCommit); err != nil && !gitHasCommit(ctx, mirrorPath, baseCommit) {
			return fmt.Errorf("base commit %s is not available in %s after direct fetch from %s: %w", baseCommit, mirrorPath, repoURL, err)
		}
	}
	if !gitHasCommit(ctx, mirrorPath, baseCommit) {
		return fmt.Errorf("base commit %s is not available in %s after fetching from %s", baseCommit, mirrorPath, repoURL)
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("create parent for checkout %s: %w", destDir, err)
	}
	if _, err := runGitCaptured(ctx, "", "clone", mirrorPath, destDir); err != nil {
		return fmt.Errorf("clone worktree from mirror %s to %s: %w", mirrorPath, destDir, err)
	}
	if _, err := runGitCaptured(ctx, destDir, "checkout", "--detach", baseCommit); err != nil {
		return fmt.Errorf("checkout base commit %s in %s: %w", baseCommit, destDir, err)
	}
	return nil
}

// EnsureBareMirror creates or refreshes the bare mirror used by GoBench git operations.
func EnsureBareMirror(ctx context.Context, repoURL, cacheDir string) (string, error) {
	if repoURL == "" {
		return "", fmt.Errorf("repoURL is empty")
	}
	if cacheDir == "" {
		return "", fmt.Errorf("cacheDir is empty")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir %s: %w", cacheDir, err)
	}
	mirrorPath := filepath.Join(cacheDir, repoSlug(repoURL)+".git")
	if _, err := os.Stat(mirrorPath); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat mirror %s: %w", mirrorPath, err)
		}
		if _, err := runGitCaptured(ctx, "", "clone", "--bare", repoURL, mirrorPath); err != nil {
			return "", fmt.Errorf("create bare mirror for %s at %s: %w", repoURL, mirrorPath, err)
		}
	} else {
		if _, err := runGitCaptured(ctx, mirrorPath, "fetch", "--all", "--prune"); err != nil {
			return "", fmt.Errorf("refresh bare mirror %s for %s: %w", mirrorPath, repoURL, err)
		}
	}
	return mirrorPath, nil
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
