package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Repro for the 2026-07-07 harness-audit item (3): under AutoVerifySoft, a
// verify TIMEOUT short-circuits straight to advisory accept (gate.go
// verifyCompletionFailure), so a merely-slow suite flips the finish green on
// the first attempt while a red suite gets autoVerifyMaxFeedback pushback
// rounds first. A timeout is inconclusive, not a pass: it must consume the
// same feedback budget as any other soft-gate failure, and only after the
// budget is exhausted may the gate accept advisorily.
func TestAutoVerifySoftTimeoutConsumesFeedbackBudget(t *testing.T) {
	cfg := Config{
		Sandbox: autoExecSandbox{Sandbox: sbWith(t, nil), exec: func(string, time.Duration) *sandbox.Result {
			return &sandbox.Result{ExitCode: 124, TimedOut: true}
		}},
		VerifyCmd:          "go test ./...",
		SkipVerifyBaseline: true,
		AutoVerifySoft:     true,
		VerifyContinue:     true,
		Obs:                nopObserver{},
	}
	g := mustNewGates(t, context.Background(), cfg, time.Second)
	in := finishInput{answer: "done", canContinue: true, verifyContinuePhrase: "you said done"}
	for i := 0; i < autoVerifyMaxFeedback; i++ {
		d := g.finish(context.Background(), in)
		if d.kind != finishContinue {
			t.Fatalf("finish %d kind = %v (outcome=%v reason=%q), want continue — a verify TIMEOUT must consume the soft-gate feedback budget, not instantly flip the gate advisory", i+1, d.kind, d.outcome, d.reason)
		}
		if !strings.Contains(d.feedback, "INCONCLUSIVE") {
			t.Fatalf("finish %d feedback should surface the timeout as inconclusive, got %q", i+1, d.feedback)
		}
	}
	if d := g.finish(context.Background(), in); d.kind != finishAnswered {
		t.Fatalf("post-budget finish kind = %v outcome=%v reason=%q, want answered (advisory accept once the budget is exhausted)", d.kind, d.outcome, d.reason)
	}
}
