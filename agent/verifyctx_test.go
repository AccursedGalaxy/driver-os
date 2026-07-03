package agent

import (
	"context"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Review #3: every closing verification gate (answer, FinishTool, kill, cap,
// deadline) shares ONE context policy via verifyRun — skip on user cancel, run
// detached past an expired deadline. These pin both halves on both gate
// functions, so the answer path and the kill/cap path can never again reach
// opposite outcomes from the same world state.

// countingExecSandbox counts Exec calls; only Exec is ever reached through
// runOp, so the embedded nil Sandbox is never touched.
type countingExecSandbox struct {
	sandbox.Sandbox
	execs int
}

func (c *countingExecSandbox) Exec(context.Context, sandbox.Command) (*sandbox.Result, error) {
	c.execs++
	return &sandbox.Result{ExitCode: 0}, nil
}

func TestVerifyGatesSkipOnUserCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the user asked us to stop — spend nothing more.
	exec := &countingExecSandbox{}
	cfg := Config{VerifySandbox: exec, VerifyCmd: "true"}

	res := upgradeIfVerified(ctx, cfg, &RunResult{Outcome: HitCap}, time.Second)
	if res.Outcome != HitCap {
		t.Errorf("a user-canceled run must not be upgraded; got %q", res.Outcome)
	}
	if reason, _ := verifyTermination(ctx, cfg, false, time.Second); reason == "" {
		t.Error("a user-canceled answer must stay unverified (non-empty reason) — the check never ran")
	}
	if exec.execs != 0 {
		t.Errorf("user cancel must skip the verify command entirely; it ran %d time(s)", exec.execs)
	}
}

func TestVerifyGatesRunDetachedPastDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done() // the budget has expired — ground truth must still be checked.
	cfg := Config{Sandbox: sbWith(t, nil), VerifyCmd: "true"}

	res := upgradeIfVerified(ctx, cfg, &RunResult{Outcome: HitDeadline}, time.Second)
	if res.Outcome != Answered {
		t.Errorf("HitDeadline on passing code must upgrade via the detached check; got %q (%s)", res.Outcome, res.Reason)
	}
	if reason, _ := verifyTermination(ctx, cfg, false, time.Second); reason != "" {
		t.Errorf("the answer path must reach the same verdict as the kill path past the deadline; got reason %q", reason)
	}
}
