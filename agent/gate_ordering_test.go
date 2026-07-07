package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// scriptedExecSandbox wraps a real sandbox and returns canned Exec results in
// call order. Every Exec call consumes one result; a call beyond the script
// panics the test.
type scriptedExecSandbox struct {
	sandbox.Sandbox
	t       *testing.T
	results []*sandbox.Result
	call    int
}

func (s *scriptedExecSandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	if s.call >= len(s.results) {
		s.t.Fatalf("unexpected Exec call %d (only %d scripted)", s.call, len(s.results))
	}
	r := s.results[s.call]
	s.call++
	return r, nil
}

// TestVerifyTerminationTimeoutAndBaselineGrace verifies that the timeout path
// and the red-baseline grace-latch path in gates.verifyTermination each work
// correctly and do not interfere. The isRunTimeout early-return and the
// baseline block operate on mutually exclusive verifyOut states (the
// package-level verifyTermination returns either an INCONCLUSIVE reason or a
// "did not pass:" reason, never both), so their relative ordering is not
// directly observable through these calls alone. What IS observable and
// tested here:
//
//   - A timeout must produce an INCONCLUSIVE reason, set noContinue=true,
//     and MUST NOT consume the baseline grace.
//   - A baseline-identical failure must append the "ALREADY failing" note
//     and grant exactly one grace round (noContinue=false on the first,
//     noContinue=true on the second after the grace is spent).
func TestVerifyTerminationTimeoutAndBaselineGrace(t *testing.T) {
	// Baseline-identical failure output: deterministic, no timestamps.
	const failOut = "FAIL: want 42, got 17\n"

	base := sbWith(t, nil)

	sb := &scriptedExecSandbox{
		Sandbox: base,
		t:       t,
		results: []*sandbox.Result{
			// 0: baseline — consumed by newGates. Genuine red, non-timeout.
			{Stdout: []byte(failOut), ExitCode: 1},
			// 1: timed-out verify.
			{TimedOut: true, ExitCode: 124},
			// 2: byte-identical to baseline.
			{Stdout: []byte(failOut), ExitCode: 1},
			// 3: byte-identical to baseline (again).
			{Stdout: []byte(failOut), ExitCode: 1},
		},
	}

	cfg := Config{
		VerifyCmd:      "go test ./...",
		VerifyContinue: true,
		Sandbox:        sb,
	}

	g := mustNewGates(t, context.Background(), cfg, 0)
	if !g.verifyBaselineRed {
		t.Fatal("verifyBaselineRed = false, want true (baseline should be red)")
	}

	// --- Call 1: timeout ---
	// The package-level verifyTermination returns an INCONCLUSIVE reason
	// because isRunTimeout(verifyOut) is true. The method-level timeout
	// early-return fires: noContinue=true, baselineGraceUsed untouched.
	outcome, reason, noContinue := g.verifyTermination(context.Background(), false)
	_ = outcome
	if reason == "" {
		t.Error("call 1 (timeout): reason is empty, want non-empty")
	}
	if !strings.Contains(reason, "INCONCLUSIVE") {
		t.Errorf("call 1 (timeout): reason does not contain INCONCLUSIVE: %q", reason)
	}
	if strings.Contains(reason, "ALREADY failing") {
		t.Errorf("call 1 (timeout): reason contains 'ALREADY failing' — the baseline note leaked into a timeout reason: %q", reason)
	}
	if !noContinue {
		t.Error("call 1 (timeout): noContinue = false, want true (timeout must suppress continue)")
	}
	if g.baselineGraceUsed {
		t.Error("call 1 (timeout): baselineGraceUsed = true, want false (timeout must not consume the grace)")
	}

	// --- Call 2: baseline-identical failure (grace round) ---
	// Package-level returns "did not pass:". The method-level baseline block
	// fires: the reason gets the "ALREADY failing" note, fingerprint matches
	// baseline → grace is granted (noContinue stays false).
	outcome, reason, noContinue = g.verifyTermination(context.Background(), false)
	_ = outcome
	if reason == "" {
		t.Error("call 2 (identical): reason is empty, want non-empty")
	}
	if !strings.Contains(reason, "ALREADY failing") {
		t.Errorf("call 2 (identical): reason does not contain 'ALREADY failing': %q", reason)
	}
	if noContinue {
		t.Error("call 2 (identical): noContinue = true, want false (grace round should grant one more try)")
	}
	if !g.baselineGraceUsed {
		t.Error("call 2 (identical): baselineGraceUsed = false, want true (grace should be consumed)")
	}

	// --- Call 3: second identical failure (grace spent → terminal) ---
	outcome, reason, noContinue = g.verifyTermination(context.Background(), false)
	_ = outcome
	if reason == "" {
		t.Error("call 3 (identical): reason is empty, want non-empty")
	}
	if !noContinue {
		t.Error("call 3 (identical): noContinue = false, want true (grace spent → terminal)")
	}
}

// TestTimedOutBaselineIsNoSignal proves that a timed-out baseline does NOT set
// verifyBaselineRed. A timed-out verify on the untouched workspace is
// inconclusive — it might pass given more time — so it must not interact with
// the red-baseline block from the other direction either.
func TestTimedOutBaselineIsNoSignal(t *testing.T) {
	base := sbWith(t, nil)

	sb := &scriptedExecSandbox{
		Sandbox: base,
		t:       t,
		results: []*sandbox.Result{
			// 0: timed-out baseline — no signal.
			{TimedOut: true, ExitCode: 124},
		},
	}

	cfg := Config{
		VerifyCmd: "go test ./...",
		Sandbox:   sb,
	}

	g := mustNewGates(t, context.Background(), cfg, 0)
	if g.verifyBaselineRed {
		t.Error("verifyBaselineRed = true, want false (timed-out baseline is no signal)")
	}
	// baselineGraceUsed must also be false (no red baseline to grant grace from).
	if g.baselineGraceUsed {
		t.Error("baselineGraceUsed = true, want false")
	}
}