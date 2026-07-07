package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyCompletionSkipsVerifyRunOnUnchangedGreenBaseline(t *testing.T) {
	ctx := context.Background()
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	tripwire := filepath.Join(t.TempDir(), "verify-tripwire")
	obs := &noteSpy{}
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		VerifyCmd: "test ! -f " + shellSingleQuote(tripwire),
		Obs:       obs,
	}
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)
	if gs.verifyBaselineRed {
		t.Fatal("verifyBaselineRed = true, want green baseline before tripwire exists")
	}
	if gs.runBaseTree == "" {
		t.Fatal("runBaseTree is empty — baseline tree was not captured")
	}
	if err := os.WriteFile(tripwire, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	decision := gs.finish(ctx, finishInput{answer: "backlog item list"})
	if decision.kind != finishAnswered {
		t.Fatalf("finish kind = %v (outcome %s, reason %q), want finishAnswered", decision.kind, decision.outcome, decision.reason)
	}
	foundNote := false
	for _, note := range obs.notes {
		if strings.Contains(note, "verify: no file changes this run") && strings.Contains(note, "baseline was green") {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Fatalf("missing unchanged-tree verify skip note; notes = %#v", obs.notes)
	}
}

func TestVerifyCompletionUnchangedRedBaselinePreservesFailureAndSkipsRun(t *testing.T) {
	ctx := context.Background()
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		VerifyCmd: `sh -c 'printf x >> verify-count.txt; exit 1'`,
		Obs:       &noteSpy{},
	}
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)
	if !gs.verifyBaselineRed {
		t.Fatal("verifyBaselineRed = false, want true from failing baseline")
	}
	if gs.runBaseTree == "" {
		t.Fatal("runBaseTree is empty — baseline tree was not captured")
	}
	countPath := filepath.Join(root, "verify-count.txt")
	before, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != "x" {
		t.Fatalf("baseline verify count file = %q, want one baseline write", before)
	}

	outcome, reason, noContinue := gs.verifyCompletion(ctx, false)
	if outcome != Unverified {
		t.Fatalf("outcome = %s, want Unverified", outcome)
	}
	if noContinue {
		t.Fatal("first identical red-baseline failure noContinue = true, want false (grace round preserved)")
	}
	if !gs.baselineGraceUsed {
		t.Fatal("baselineGraceUsed = false, want true after first identical red-baseline failure")
	}
	if !strings.Contains(reason, "did not pass:") || !strings.Contains(reason, "ALREADY failing before any changes") {
		t.Fatalf("reason missing red-baseline wording:\n%s", reason)
	}
	after, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "x" {
		t.Fatalf("verify command ran on unchanged red-baseline finish; count file = %q, want baseline write only", after)
	}

	outcome, reason, noContinue = gs.verifyCompletion(ctx, false)
	if outcome != Unverified || reason == "" || !noContinue {
		t.Fatalf("second identical red-baseline failure = (%s, %q, %v), want terminal Unverified", outcome, reason, noContinue)
	}
	afterSecond, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSecond) != "x" {
		t.Fatalf("verify command ran on second unchanged red-baseline finish; count file = %q, want baseline write only", afterSecond)
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
