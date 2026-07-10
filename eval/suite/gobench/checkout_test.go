package gobench

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCheckoutBaseLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
	root := t.TempDir()
	repoDir := makeOrigin(t, filepath.Join(root, "src"))
	base := strings.TrimSpace(gitOut(t, repoDir, "rev-parse", "HEAD"))
	destDir := filepath.Join(root, "checkout")
	if err := CheckoutBase(context.Background(), repoDir, base, destDir, filepath.Join(root, "cache")); err != nil {
		t.Fatalf("CheckoutBase: %v", err)
	}
	gotBytes, err := os.ReadFile(filepath.Join(destDir, "README.md"))
	if err != nil {
		t.Fatalf("read checked-out file: %v", err)
	}
	if got := string(gotBytes); got != "hello\n" {
		t.Fatalf("checked-out README = %q", got)
	}
}

func TestEnsureBareMirrorConcurrentColdCache(t *testing.T) {
	origin := makeOrigin(t, filepath.Join(t.TempDir(), "origin"))
	cache := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	paths := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := EnsureBareMirror(context.Background(), origin, cache)
			errs <- err
			paths <- p
		}()
	}
	wg.Wait()
	close(errs)
	close(paths)
	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureBareMirror raced: %v", err)
		}
	}
	var first string
	for p := range paths {
		if first == "" {
			first = p
		} else if p != first {
			t.Fatalf("paths differ: %q vs %q", p, first)
		}
	}
	if err := validateBareMirror(context.Background(), first); err != nil {
		t.Fatalf("mirror invalid: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(cache, "*.git"))
	if len(matches) != 1 {
		t.Fatalf("mirrors = %v, want exactly one", matches)
	}
}

func TestEnsureBareMirrorInterruptedCloneCleanupAndRecovery(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	cache := filepath.Join(root, "cache")
	_, err := EnsureBareMirror(context.Background(), origin, cache)
	if err == nil {
		t.Fatal("first call unexpectedly succeeded")
	}
	var fe *FixtureError
	if !errors.As(err, &fe) {
		t.Fatalf("error %T, want FixtureError", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(cache, "*.tmp-*")); len(leftovers) != 0 {
		t.Fatalf("temp leftovers: %v", leftovers)
	}
	makeOrigin(t, origin)
	p, err := EnsureBareMirror(context.Background(), origin, cache)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if err := validateBareMirror(context.Background(), p); err != nil {
		t.Fatalf("mirror invalid after recovery: %v", err)
	}
}

func TestEnsureBareMirrorPoisonedCacheRecovery(t *testing.T) {
	origin := makeOrigin(t, filepath.Join(t.TempDir(), "origin"))
	cache := t.TempDir()
	poison := filepath.Join(cache, repoSlug(origin)+".git")
	if err := os.MkdirAll(poison, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poison, "garbage"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := EnsureBareMirror(context.Background(), origin, cache)
	if err != nil {
		t.Fatalf("EnsureBareMirror: %v", err)
	}
	if p != poison {
		t.Fatalf("path = %q, want %q", p, poison)
	}
	if err := validateBareMirror(context.Background(), p); err != nil {
		t.Fatalf("recovered mirror invalid: %v", err)
	}
}

func TestEnsureBareMirrorWarmCacheReuse(t *testing.T) {
	origin := makeOrigin(t, filepath.Join(t.TempDir(), "origin"))
	cache := t.TempDir()
	p1, err := EnsureBareMirror(context.Background(), origin, cache)
	if err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(p1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := EnsureBareMirror(context.Background(), origin, cache)
	if err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(p2)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("paths differ")
	}
	if !os.SameFile(info1, info2) {
		t.Fatalf("warm cache was replaced")
	}
}

func TestDefaultCacheDirUsesUserCache(t *testing.T) {
	got := DefaultCacheDir()
	if strings.HasPrefix(got, os.TempDir()+string(os.PathSeparator)) || got == filepath.Join(os.TempDir(), "gobench-cache") {
		t.Fatalf("DefaultCacheDir() = %q; want user cache backed path when UserCacheDir works", got)
	}
	if !strings.HasSuffix(got, filepath.Join("driver-os", "gobench")) {
		t.Fatalf("DefaultCacheDir() = %q", got)
	}
}

func TestBoardReadyAcceptRefuse(t *testing.T) {
	if err := BoardReady([]Verdict{{InstanceID: "ok", Resolved: true}}); err != nil {
		t.Fatalf("ready rejected: %v", err)
	}
	err := BoardReady([]Verdict{{InstanceID: "bad", Infra: true, InfraCause: "disk full"}})
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("BoardReady err = %v", err)
	}
	if got := ResolveDenominator([]Verdict{{}, {Infra: true}}); got != 1 {
		t.Fatalf("denominator = %d", got)
	}
}

func makeOrigin(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@example.com")
	run(t, dir, "git", "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "README.md")
	run(t, dir, "git", "commit", "-m", "initial")
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, dir, err, out)
	}
}
