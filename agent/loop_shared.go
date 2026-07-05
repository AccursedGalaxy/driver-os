package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

type loopKnobs struct {
	maxIter      int
	maxTok       int
	runTimeout   time.Duration
	spiralWindow int
}

func resolveKnobs(cfg Config) loopKnobs {
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}
	maxTok := cfg.MaxTokens
	if maxTok <= 0 {
		maxTok = DefaultMaxTokens
	}
	runTimeout := cfg.RunTimeout
	if runTimeout <= 0 {
		runTimeout = defaultRunTimeout
	}
	spiralWindow := cfg.NavSpiralWindow
	if spiralWindow <= 0 {
		spiralWindow = noProgressWindow
	}
	return loopKnobs{
		maxIter:      maxIter,
		maxTok:       maxTok,
		runTimeout:   runTimeout,
		spiralWindow: spiralWindow,
	}
}

func wrapTools(cfg Config, runTimeout time.Duration) map[string]Tool {
	tools := cfg.Tools
	if tools == nil {
		tools = DefaultTools(cfg.Sandbox, runTimeout)
	}
	// (REVIEW-GATE slice 0) The test fence wraps the mutation tools BEFORE the
	// system prompt is built from them, and the closing gates snapshot their
	// run-start state (fence hashes + the review diff base). Empty fence + nil
	// Reviewer ⇒ both are inert and behavior is byte-identical.
	// Diff-scope wraps FIRST, test-fence LAST, so the fence wins for fenced
	// in-scope paths (the refusal names whichever mechanism refused).
	tools = applyDiffScope(tools, cfg.DiffScope, cfg.Sandbox)
	tools = applyTestFence(tools, cfg.TestFence, cfg.Sandbox)
	return tools
}

func resolveAutoVerify(ctx context.Context, cfg *Config) {
	if !cfg.AutoVerify || cfg.VerifyCmd != "" || cfg.Root == "" {
		return
	}
	if cfg.MinIsolation > sandbox.IsolationNone {
		cfg.Obs.Note("auto-verify: off for untrusted isolation; supply -verify-cmd to arm an explicit gate")
		return
	}
	cmd, prov := deriveVerifyCmd(ctx, cfg.verifySandbox(), cfg.Root)
	if cmd == "" {
		return
	}
	cfg.Obs.Note(fmt.Sprintf("verify gate auto-derived from %s: `%s`", prov, cmd))
	out, err := runOp(context.WithoutCancel(ctx), cfg.verifySandbox(), cmd, autoVerifyBaselineTimeout)
	cfg.SkipVerifyBaseline = true
	if err != nil {
		cfg.Obs.Note(fmt.Sprintf("auto-verify: disarmed `%s` because the baseline could not run: %v", cmd, err))
		return
	}
	if isRunFailure(out) {
		if isRunTimeout(out) {
			cfg.Obs.Note(fmt.Sprintf("auto-verify: disarmed `%s` because the baseline timed out after %s", cmd, autoVerifyBaselineTimeout))
		} else {
			cfg.Obs.Note(fmt.Sprintf("auto-verify: disarmed `%s` because it is already red on the untouched workspace", cmd))
		}
		return
	}
	cfg.VerifyCmd = cmd
	cfg.AutoVerifySoft = true
	cfg.VerifyContinue = true
	cfg.autoVerifyProvenance = prov
	cfg.Obs.Note(fmt.Sprintf("auto-verify: armed soft verify gate `%s` (derived from %s)", cmd, prov))
}

func redBaselineRefusal(cfg Config, gs *gates) *RunResult {
	// AbortOnRedBaseline: if the baseline is red and the caller wants us to stop,
	// return immediately before any model call — the gate is unsatisfiable at base.
	if cfg.AbortOnRedBaseline && gs.verifyBaselineRed {
		res := &RunResult{}
		res.Outcome = Unverified
		res.Reason = fmt.Sprintf(
			"refused to run: the verify command %q is ALREADY failing on the untouched workspace — "+
				"the gate is unsatisfiable at base (-verify-baseline=abort caused this refusal)",
			cfg.VerifyCmd,
		)
		res.Iterations = 0
		return res
	}
	return nil
}

func classifyGenerateErr(ctx, loopCtx context.Context, cfg Config, err error) (outcome Outcome, reason string, returnErr error, ok bool) {
	// A deadline hit mid-Generate is the wall-clock budget, not a transport fault.
	if cfg.MaxWallClock > 0 && loopCtx.Err() == context.DeadlineExceeded {
		return HitDeadline, fmt.Sprintf("hit wall-clock budget (%s) mid-turn", cfg.MaxWallClock), nil, true
	}
	// (HP-1) The window overflowed and eviction couldn't compact it any
	// further (only TASK + the most recent turn remain). Degrade gracefully
	// to a typed stop rather than surfacing it as a transport fault.
	if errors.Is(err, llm.ErrContextLength) {
		return HitContextLimit, "context window exceeded and could not be compacted further", nil, true
	}
	// A caller cancel is a normal typed stop, not an infrastructure error.
	// Check the PARENT ctx — loopCtx may also carry DeadlineExceeded from
	// MaxWallClock, and wall-clock expiry must read HitDeadline, not Canceled.
	// We check ctx.Err() because signal.NotifyContext (Go 1.26+) cancels with
	// a custom signalError cause, and Err() is cause-agnostic while still
	// distinguishing deadline expiry.
	if errors.Is(ctx.Err(), context.Canceled) {
		return Canceled, "run canceled by the caller (interrupt)", nil, true
	}
	// A transport/auth failure is a real stop (tool errors are not — see
	// dispatch). Record it as a typed outcome AND return the error.
	return ProviderErr, err.Error(), err, true
}
