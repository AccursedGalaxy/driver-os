package agent

// Empty-diff answered hole (harness audit 2026-07-07 / VISION.md): on a
// change-requiring task (-require-diff), a no-op "solve" must never exit
// Answered — on ANY finish path, including a trusted FinishTool call.
// require-diff is an explicit orchestrator demand and outranks the caller's
// vouch: trusting the caller about task completion cannot manufacture a diff
// that does not exist.

import (
	"context"
	"strings"
	"testing"
)

// A trusted finish (FinishToolTrustsCaller) currently skips verifyCompletion
// entirely, so RequireDiff is silently dropped. It must still be enforced.
func TestTrustedFinishStillEnforcesRequireDiff(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	cfg := Config{
		Sandbox:     sb,
		Root:        root,
		RequireDiff: true,
		Obs:         &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)
	if gs.runBaseTree == "" {
		t.Fatal("runBaseTree is empty — require-diff baseline tree was not captured")
	}

	decision := gs.finish(ctx, finishInput{answer: "done", trusted: true})
	if decision.kind == finishAnswered {
		t.Fatal("trusted finish with an unchanged tree finished Answered — require-diff must bind trusted finishes too")
	}
	if decision.outcome != Unverified {
		t.Fatalf("outcome = %q (reason %q), want Unverified", decision.outcome, decision.reason)
	}
	if !strings.Contains(decision.reason, "require-diff") {
		t.Fatalf("reason = %q, want it to name require-diff", decision.reason)
	}
}

// Regression pin for the untrusted answer path: require-diff on an unchanged
// tree is Unverified (this already holds today — pinned so the trusted-path
// fix cannot regress it).
func TestAnswerFinishEnforcesRequireDiff(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	cfg := Config{
		Sandbox:     sb,
		Root:        root,
		RequireDiff: true,
		Obs:         &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	decision := gs.finish(ctx, finishInput{answer: "done"})
	if decision.kind == finishAnswered {
		t.Fatal("answer finish with an unchanged tree finished Answered despite require-diff")
	}
	if decision.outcome != Unverified || !strings.Contains(decision.reason, "require-diff") {
		t.Fatalf("outcome/reason = %q/%q, want Unverified with a require-diff reason", decision.outcome, decision.reason)
	}
}

// A trusted finish on a tree that DID change stays Answered — require-diff
// enforcement must not turn into a blanket distrust of trusted finishes.
func TestTrustedFinishWithRealDiffStaysAnswered(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	cfg := Config{
		Sandbox:     sb,
		Root:        root,
		RequireDiff: true,
		Obs:         &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	if err := sb.WriteFile(ctx, "new.go", []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decision := gs.finish(ctx, finishInput{answer: "done", trusted: true})
	if decision.kind != finishAnswered {
		t.Fatalf("finish kind = %v (outcome %s, reason %q), want finishAnswered — the tree changed, require-diff is satisfied", decision.kind, decision.outcome, decision.reason)
	}
}
