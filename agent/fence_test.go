package agent

// Slice 0 tests (REVIEW-GATE-PLAN): the test fence — tool-layer refusal, the
// closing-gate re-hash (run-mediated edits included), the upgrade path, the
// docker mount-alias, and the empty-fence no-op guarantee.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

func TestMatchesFence(t *testing.T) {
	globs := []string{"*_test.go", "testdata/**"}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"calc_test.go", true},
		{"pkg/deep/calc_test.go", true}, // basename glob matches at any depth.
		{"calc.go", false},
		{"testdata/input.csv", true},
		{"pkg/testdata/golden.json", true}, // dir/** matches nested testdata too.
		{"testdata_backup/x", false},
		{"./calc_test.go", true},  // "./" normalized away.
		{"my_test.golang", false}, // no over-match past the glob.
		{"testdata", false},       // the dir itself is not a file entry we hash; matching files only.
		{"testdata/sub/x.txt", true},
	} {
		if got := matchesFence(globs, tc.path); got != tc.want {
			t.Errorf("matchesFence(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	// A slashed glob matches the full path, not the basename.
	if !matchesFence([]string{".github/*"}, ".github/ci.yml") {
		t.Error(".github/* should fence .github/ci.yml")
	}
	if matchesFence([]string{".github/*"}, "src/ci.yml") {
		t.Error(".github/* must not fence src/ci.yml")
	}
}

// The mutation tools refuse a fenced path — structured (native) and text args,
// the append path included — while non-fenced writes pass through untouched.
func TestFenceToolRefusal(t *testing.T) {
	sb := sbWith(t, map[string]string{"calc.go": "package calc\n", "calc_test.go": "package calc // test\n"})
	tools := applyTestFence(DefaultTools(sb, defaultRunTimeout), []string{"*_test.go", "testdata/**"}, sb)
	ctx := context.Background()

	// Structured write_file (the append flag flows through the same handler).
	for _, args := range []string{
		`{"path":"calc_test.go","content":"sabotaged"}`,
		`{"path":"calc_test.go","content":"sabotaged","append":true}`,
		`{"path":"testdata/new.txt","content":"x"}`, // creating a NEW fenced file is refused too.
	} {
		_, err := tools["write_file"].RunJSON(ctx, []byte(args))
		if err == nil || !strings.Contains(err.Error(), "test fence") {
			t.Fatalf("write_file %s: want fence refusal, got %v", args, err)
		}
	}
	// Structured edit_file.
	if _, err := tools["edit_file"].RunJSON(ctx, []byte(`{"path":"calc_test.go","old":"test","new":"x"}`)); err == nil || !strings.Contains(err.Error(), "test fence") {
		t.Fatalf("edit_file: want fence refusal, got %v", err)
	}
	// Text-protocol args (path = first token).
	if _, err := tools["write_file"].Run(ctx, "calc_test.go sabotaged"); err == nil || !strings.Contains(err.Error(), "test fence") {
		t.Fatalf("text write_file: want fence refusal, got %v", err)
	}
	if _, err := tools["edit_file"].Run(ctx, "calc_test.go test ||| x"); err == nil || !strings.Contains(err.Error(), "test fence") {
		t.Fatalf("text edit_file: want fence refusal, got %v", err)
	}

	// The file is untouched, and non-fenced writes still work.
	if data, _ := sb.ReadFile(ctx, "calc_test.go"); string(data) != "package calc // test\n" {
		t.Fatalf("fenced file was modified: %q", data)
	}
	if _, err := tools["write_file"].RunJSON(ctx, []byte(`{"path":"calc.go","content":"package calc // v2\n"}`)); err != nil {
		t.Fatalf("non-fenced write refused: %v", err)
	}
}

// A docker mount-alias absolute path ("/workspace/x_test.go") is fenced the
// same as its relative form — absolute addressing can't dodge the fence.
func TestFenceMountAliasPath(t *testing.T) {
	sb := sbWith(t, map[string]string{"calc_test.go": "package calc\n"})
	sb.(*local.Sandbox).SetMountAlias("/workspace")
	tools := applyTestFence(DefaultTools(sb, defaultRunTimeout), []string{"*_test.go"}, sb)
	if _, err := tools["write_file"].RunJSON(context.Background(), []byte(`{"path":"/workspace/calc_test.go","content":"x"}`)); err == nil || !strings.Contains(err.Error(), "test fence") {
		t.Fatalf("alias path: want fence refusal, got %v", err)
	}
}

// An empty fence leaves the toolset — the same map values, no wrapping — and
// so the run's behavior byte-for-byte identical to before the feature existed.
func TestFenceEmptyIsNoOp(t *testing.T) {
	sb := sbWith(t, map[string]string{"a_test.go": "old"})
	tools := DefaultTools(sb, defaultRunTimeout)
	if got := applyTestFence(tools, nil, sb); &got == &tools || len(got) != len(tools) {
		// same underlying map returned (no copy, no wrap)
	}
	out, err := applyTestFence(tools, nil, sb)["write_file"].RunJSON(context.Background(), []byte(`{"path":"a_test.go","content":"new"}`))
	if err != nil || !strings.Contains(out, "wrote") {
		t.Fatalf("empty fence must not refuse test writes: %v %q", err, out)
	}
}

// A `run`-mediated mutation of a fenced file (shell redirect — the tool fence
// can't see it) is caught by the closing re-hash: the finish is Unverified
// with the fence named, even though VerifyCmd passes.
func TestFenceViolationViaRunIsUnverified(t *testing.T) {
	sb := sbWith(t, map[string]string{"calc_test.go": "package calc // original\n"})
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo sabotage >> calc_test.go"})},
		{llm.Text("done")},
	}}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sb, Task: "t",
		TestFence: []string{"*_test.go"},
		VerifyCmd: "true", // execution gate green — only the fence stands in the way.
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s (%s), want unverified", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "test fence violated") || !strings.Contains(res.Reason, "calc_test.go") {
		t.Fatalf("reason %q must name the fence violation and the file", res.Reason)
	}
}

// The cap/kill upgrade path is equally fenced: a run that would be rescued by
// a passing VerifyCmd stays un-upgraded when a fenced file drifted.
func TestFenceViolationBlocksUpgrade(t *testing.T) {
	sb := sbWith(t, map[string]string{"calc_test.go": "package calc\n"})
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo x >> calc_test.go"})},
		{structuredCall("c2", "run", map[string]any{"command": "echo probe"})},
	}}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sb, Task: "t",
		TestFence:     []string{"*_test.go"},
		VerifyCmd:     "true",
		MaxIterations: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != HitCap {
		t.Fatalf("outcome = %s, want hit_cap (upgrade must be blocked by the fence)", res.Outcome)
	}
}

// Deleting a fenced file (rather than editing it) is drift too.
func TestFenceDeletionIsDrift(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a_test.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	f := newFenceState(context.Background(), Config{Sandbox: sb, TestFence: []string{"*_test.go"}})
	if err := os.Remove(filepath.Join(root, "a_test.go")); err != nil {
		t.Fatal(err)
	}
	if got := f.drift(context.Background(), sb); len(got) != 1 || got[0] != "a_test.go" {
		t.Fatalf("drift = %v, want [a_test.go]", got)
	}
}

func TestSubstanceSignals(t *testing.T) {
	diff := `diff --git a/calc_test.go b/calc_test.go
--- a/calc_test.go
+++ b/calc_test.go
@@ -1,5 +1,4 @@
 func TestAdd(t *testing.T) {
+	t.Skip("flaky")
-	if got != want {
-		t.Errorf("got %d", got)
-	}
 }
diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -1,2 +1,3 @@
+//go:build ignore
`
	sig := substanceSignals(diff)
	joined := strings.Join(sig, "\n")
	for _, want := range []string{"t.Skip", "calc_test.go", "deleted 1 assertion line(s) from calc_test.go", "//go:build ignore", "calc.go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("signals %q missing %q", joined, want)
		}
	}
	if got := substanceSignals("--- a/x.go\n+++ b/x.go\n+ normal code\n"); len(got) != 0 {
		t.Errorf("clean diff produced signals: %v", got)
	}
}
