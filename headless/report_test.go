package headless

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("original\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("pre-existing dirt\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	// Pre-existing dirt: present BEFORE the snapshot, must be excluded from the report diff.
	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("pre-existing dirt CHANGED\n"), 0o644)
	return dir
}

func TestReportDeltaExcludesPreExistingDirt(t *testing.T) {
	repo := initTestRepo(t)
	delta, err := captureReportDelta(repo)
	if err != nil {
		t.Fatal(err)
	}
	// The "run" edits a tracked file, further edits the already-dirty file, and adds an untracked one.
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("edited by run\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("run touched the dirty file too\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("brand new\n"), 0o644)

	paths, err := delta.changedPaths(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(paths, ",")
	if got != "a.txt,dirty.txt,new.txt" {
		t.Fatalf("changedPaths = %q, want a.txt,dirty.txt,new.txt", got)
	}
}

func TestReportDeltaUntouchedDirtStaysOut(t *testing.T) {
	repo := initTestRepo(t)
	delta, err := captureReportDelta(repo)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("edited by run\n"), 0o644)
	paths, err := delta.changedPaths(repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "a.txt" {
		t.Fatalf("untouched pre-existing dirt leaked into the delta: %v", paths)
	}
}

func TestWriteReportPatchMode(t *testing.T) {
	dir := t.TempDir()
	patch := filepath.Join(dir, "run-1.patch")
	os.WriteFile(patch, []byte("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n"), 0o644)
	ans := "did the thing"
	wt := "/tmp/driver-agent-wt-keep"
	r := resultObject{
		ID: "run-1", Outcome: "answered", ExitCode: 0,
		Answer: &ans, PatchPath: &patch, WorktreePath: &wt,
	}
	out := filepath.Join(dir, "report.md")
	if err := writeReport(out, r, reportOpts{TraceFile: "/tmp/t.trace"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	s := string(got)
	for _, want := range []string{
		"# driver-agent report (exit=0)",
		"(full live trace: /tmp/t.trace)",
		"## result",
		"## answer",
		"did the thing",
		"## diff (harness patch: " + patch + ")",
		"+new",
		"git apply '" + patch + "'",
		"git worktree remove --force '" + wt + "'",
		"ledger.sh mark 'run-1' landed",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q\n---\n%s", want, s)
		}
	}
	for _, warning := range []string{"WARNING: outcome is", "UNREVIEWED", "did NOT pass the verify gate"} {
		if strings.Contains(s, warning) {
			t.Errorf("answered report unexpectedly contains %q\n---\n%s", warning, s)
		}
	}
}

func TestWriteReportPatchModeHitCapUnreviewed(t *testing.T) {
	dir := t.TempDir()
	patch := filepath.Join(dir, "run-hit-cap.patch")
	if err := os.WriteFile(patch, []byte("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := resultObject{ID: "run-hit-cap", Outcome: "hit_cap", PatchPath: &patch}
	out := filepath.Join(dir, "report.md")
	if err := writeReport(out, r, reportOpts{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		"WARNING: outcome is hit_cap — this patch did NOT pass the verify gate.",
		"WARNING: this patch is UNREVIEWED — no review gate ran on it.",
		"git apply '" + patch + "'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q\n---\n%s", want, s)
		}
	}
}

func TestWriteReportDeltaMode(t *testing.T) {
	repo := initTestRepo(t)
	delta, err := captureReportDelta(repo)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("edited by run\n"), 0o644)
	ans := "edited a.txt"
	r := resultObject{ID: "run-2", Outcome: "answered", Answer: &ans}
	out := filepath.Join(t.TempDir(), "report.md")
	if err := writeReport(out, r, reportOpts{Repo: repo, Delta: delta}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	s := string(got)
	if !strings.Contains(s, "## files changed by this run") || !strings.Contains(s, "a.txt") {
		t.Errorf("delta report missing changed-files section:\n%s", s)
	}
	if !strings.Contains(s, "edited by run") {
		t.Errorf("delta report missing the diff content:\n%s", s)
	}
	if strings.Contains(s, "pre-existing dirt CHANGED") {
		t.Errorf("delta report leaked pre-existing dirt:\n%s", s)
	}
}
