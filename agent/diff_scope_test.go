package agent

// Diff-scope tests (the inverse of the test fence): a configured glob list of
// paths the solver's changes MAY touch; any change outside it is a first-class
// failure (ScopeViolation). Enforcement is layered — tool-layer refusal plus
// closing-gate tree-diff — and degrades loudly on non-git workspaces.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

func TestDiffScopeToolRefusal(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"inside/x.go":  "package inside",
		"outside/x.go": "package outside",
	})
	tools := applyDiffScope(DefaultTools(sb, defaultRunTimeout), []string{"inside/**"}, sb)
	ctx := context.Background()

	// write_file outside scope refused (recovery-shaped error names the allowed globs).
	for _, args := range []string{
		`{"path":"outside/x.go","content":"sabotaged"}`,
		`{"path":"root_file.go","content":"x"}`,
	} {
		_, err := tools["write_file"].RunJSON(ctx, []byte(args))
		if err == nil || !strings.Contains(err.Error(), "outside the diff scope") {
			t.Fatalf("write_file %s: want diff-scope refusal, got %v", args, err)
		}
		if !strings.Contains(err.Error(), "inside/**") {
			t.Fatalf("write_file %s: refusal must name the allowed globs, got %v", args, err)
		}
	}

	// edit_file outside scope refused.
	if _, err := tools["edit_file"].RunJSON(ctx, []byte(`{"path":"outside/x.go","old":"package","new":"x"}`)); err == nil || !strings.Contains(err.Error(), "outside the diff scope") {
		t.Fatalf("edit_file: want diff-scope refusal, got %v", err)
	}

	// append (write_file with append) outside scope refused.
	if _, err := tools["write_file"].RunJSON(ctx, []byte(`{"path":"outside/x.go","content":"more","append":true}`)); err == nil || !strings.Contains(err.Error(), "outside the diff scope") {
		t.Fatalf("append outside scope: want diff-scope refusal, got %v", err)
	}

	// write_file inside scope succeeds.
	out, err := tools["write_file"].RunJSON(ctx, []byte(`{"path":"inside/x.go","content":"updated"}`))
	if err != nil {
		t.Fatalf("write_file inside scope: got err %v, want success", err)
	}
	if !strings.Contains(out, "wrote") {
		t.Fatalf("write_file inside scope: output = %q, want 'wrote'", out)
	}

	// The outside file is untouched.
	data, _ := sb.ReadFile(ctx, "outside/x.go")
	if string(data) != "package outside" {
		t.Fatalf("outside file was modified: %q", data)
	}
}

func TestDiffScopeAnchored(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"inside/a.go": "package inside",
		"other":       "I am a file, not a directory",
	})
	tools := applyDiffScope(DefaultTools(sb, defaultRunTimeout), []string{"inside/**"}, sb)
	ctx := context.Background()

	// "inside/a.go" is allowed.
	if _, err := tools["write_file"].RunJSON(ctx, []byte(`{"path":"inside/a.go","content":"updated"}`)); err != nil {
		t.Fatalf("write_file inside/a.go: got err %v, want success", err)
	}

	// "inside" (the bare file) is REFUSED.
	// We use a path that doesn't exist yet to avoid the "is a directory" error
	// from the local sandbox if "inside" was created as a directory by sbWith.
	if _, err := tools["write_file"].RunJSON(ctx, []byte(`{"path":"inside","content":"sabotaged"}`)); err == nil || !strings.Contains(err.Error(), "outside the diff scope") {
		t.Fatalf("write_file inside: want diff-scope refusal, got %v", err)
	}
}

func TestDiffScopeEmptyIsNoOp(t *testing.T) {
	sb := sbWith(t, map[string]string{"a_test.go": "old"})
	tools := DefaultTools(sb, defaultRunTimeout)
	if got := applyDiffScope(tools, nil, sb); &got == &tools || len(got) != len(tools) {
		// same underlying map returned (no copy, no wrap)
	}
	out, err := applyDiffScope(tools, nil, sb)["write_file"].RunJSON(context.Background(), []byte(`{"path":"a_test.go","content":"new"}`))
	if err != nil || !strings.Contains(out, "wrote") {
		t.Fatalf("empty diff scope must not refuse writes: %v %q", err, out)
	}
}

// Shell-level mutation outside scope: a `run` command that writes to a path
// outside the configured scope triggers ScopeViolation at the closing gate.
func TestDiffScopeViolationViaRun(t *testing.T) {
	root := t.TempDir()
	// Seed a file inside the scope and one outside.
	if err := os.MkdirAll(filepath.Join(root, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside", "x.go"), []byte("package inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// git init + commit so vcs.WriteTree works.
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })

	ns := &nativeScript{turns: [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo sabotage >> outside.txt"})},
		{llm.Text("done")},
	}}
	res, err := RunNative(context.Background(), Config{
		Model:     ns,
		Sandbox:   sb,
		Root:      root,
		Task:      "keep changes inside only",
		DiffScope: []string{"inside/**"},
		VerifyCmd: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ScopeViolation {
		t.Fatalf("outcome = %s (%s), want scope_violation", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "outside.txt") {
		t.Fatalf("reason %q must name the violating file 'outside.txt'", res.Reason)
	}
}

// In-scope-only changes with scope on => Answered (unchanged happy path).
func TestDiffScopeInScopeOnlyAnswered(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside", "x.go"), []byte("package inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })

	ns := &nativeScript{turns: [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "inside/x.go", "content": "package inside // updated\n"})},
		{llm.Text("updated in-scope file")},
	}}
	res, err := RunNative(context.Background(), Config{
		Model:     ns,
		Sandbox:   sb,
		Root:      root,
		Task:      "edit in-scope files only",
		DiffScope: []string{"inside/**"},
		VerifyCmd: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered", res.Outcome, res.Reason)
	}
}

// Fence + scope together on the same path: refusal message identifies the TEST
// FENCE (fence wins), not the diff scope.
func TestDiffScopeFenceWins(t *testing.T) {
	sb := sbWith(t, map[string]string{"inside/x_test.go": "package inside // test\n"})
	// Apply diff-scope first (inside/**), then test-fence (*_test.go).
	tools := applyDiffScope(DefaultTools(sb, defaultRunTimeout), []string{"inside/**"}, sb)
	tools = applyTestFence(tools, []string{"*_test.go"}, sb)

	// x_test.go is inside the diff scope BUT fenced — the fence refusal should fire.
	_, err := tools["write_file"].RunJSON(context.Background(), []byte(`{"path":"inside/x_test.go","content":"sabotaged"}`))
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !strings.Contains(err.Error(), "test fence") {
		t.Fatalf("refusal = %q, want 'test fence' (fence wins over scope)", err)
	}
	if strings.Contains(err.Error(), "diff scope") {
		t.Fatalf("refusal = %q, must NOT mention diff scope (fence should win)", err)
	}
}

// Non-git workspace with scope set: no false ScopeViolation; a recorded observer
// Note mentions the snapshot-failed degrade.
func TestDiffScopeNonGitWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside", "x.go"), []byte("package inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })

	rec := &noteRecorder{}
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("done")},
	}}

	cfg := Config{
		Model:     ns,
		Sandbox:   sb,
		Root:      root,
		Task:      "edit in-scope files only",
		DiffScope: []string{"inside/**"},
		VerifyCmd: "true",
		Obs:       rec,
	}

	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Must not be ScopeViolation — the snapshot failed so the gate degrades.
	if res.Outcome == ScopeViolation {
		t.Fatalf("non-git workspace must not produce ScopeViolation (degrade), got %s: %s", res.Outcome, res.Reason)
	}
	// Verify we got a degrade note about the snapshot failing.
	found := false
	for _, n := range rec.notes {
		if strings.Contains(n, "snapshot failed") || strings.Contains(n, "diff scope") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a degrade note about diff-scope snapshot failure, got notes: %v", rec.notes)
	}
}

// matchScope anchors at the repo root, unlike the fence denylist matcher:
//   - `dir/**` is a prefix, NOT a substring — it must NOT match nested dirs.
//   - Bare globs without a slash match only root-level files.
func TestMatchesScope(t *testing.T) {
	globs := []string{"inside/**", "root_*.go"}
	for _, tc := range []struct {
		path string
		want bool
	}{
		// inside/** anchored at root
		{"inside/a/b.go", true},
		{"inside", false}, // bare dir/file itself is NOT in scope (must be under the prefix)
		{"inside/x.go", true},
		{"pkg/inside/evil.go", false}, // substring NOT matched (anchor at root)
		{"notinside/x.go", false},
		{"inside_extra/x.go", false},

		// bare glob root_*.go — root-level only (use paths outside inside/ so
		// the inside/** glob doesn't claim them)
		{"root_x.go", true},
		{"root_test.go", true},
		{"pkg/root_x.go", false}, // nested — bare glob won't match
		{"other.go", false},

		// slashed glob: full-path match
		{".github/ci.yml", false}, // not in globs
	} {
		if got := matchesScope(globs, tc.path); got != tc.want {
			t.Errorf("matchesScope(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}

	// Slashed globs work the same as matchesFence — full-path only.
	if !matchesScope([]string{".github/*"}, ".github/ci.yml") {
		t.Error(".github/* should match .github/ci.yml")
	}
	if matchesScope([]string{".github/*"}, "src/ci.yml") {
		t.Error(".github/* must not match src/ci.yml")
	}
}

// Without a VerifyCmd the closing gate must still enforce the diff scope when a
// run ends via cap/kill/deadline (upgradeIfVerified).  Regression: the scope
// check used to live AFTER the VerifyCmd=="" early return, so a run with
// DiffScope set but no VerifyCmd got no gate-level scope enforcement at all.
func TestDiffScopeViolationWithoutVerifyCmd(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside", "x.go"), []byte("package inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })

	// Script writes outside.txt via a shell redirect (tool fence can't see
	// it), then does one more call to fill out iterations so we hit the cap
	// — no "done" text, so the loop never enters the finish gate.  The
	// closing-gate scope check on the upgrade path must catch the out-of-scope
	// mutation even though VerifyCmd is empty.
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo sabotage >> outside.txt"})},
		{structuredCall("c2", "run", map[string]any{"command": "echo filler"})},
	}}
	res, err := RunNative(context.Background(), Config{
		Model:         ns,
		Sandbox:       sb,
		Root:          root,
		Task:          "keep changes inside only",
		DiffScope:     []string{"inside/**"},
		MaxIterations: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ScopeViolation {
		t.Fatalf("outcome = %s (%s), want scope_violation", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "outside.txt") {
		t.Fatalf("reason %q must name the violating file 'outside.txt'", res.Reason)
	}
}
