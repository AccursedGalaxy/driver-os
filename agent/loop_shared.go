package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// obsDedupMinLen is the floor below which deduping can't save tokens: the stub
// itself is ~35 chars, and tiny observations aren't worth a map entry.
const obsDedupMinLen = 200

// obsDedup records the first iteration at which each raw observation byte-string
// was seen in a run, so a later byte-identical observation can be replaced on the
// wire (the BILLED copy) by a one-line stub. It is per-run state (create one per
// Run/RunNative call). It is purely a token optimization: it never changes whether
// a tool executes, and callers must feed it the RAW observation bytes AFTER the
// repeat-detector fingerprint has already consumed them.
type obsDedup struct {
	seen map[string]int // sha256(rawObs) -> first iteration index
}

func newObsDedup() *obsDedup { return &obsDedup{seen: map[string]int{}} }

// stub returns (stubText, true) if rawObs's bytes were already recorded at an
// earlier iteration; otherwise it records rawObs at iteration i and returns
// ("", false). Observations shorter than obsDedupMinLen are never deduped (and
// never recorded) — the stub couldn't save tokens.
func (d *obsDedup) stub(rawObs string, i int) (string, bool) {
	if len(rawObs) < obsDedupMinLen {
		return "", false
	}
	sum := sha256.Sum256([]byte(rawObs))
	key := string(sum[:])
	if k, ok := d.seen[key]; ok {
		return fmt.Sprintf("[identical to iter %d — deduped]", k), true
	}
	d.seen[key] = i
	return "", false
}

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
		tools = DefaultTools(cfg.Sandbox, runTimeout, ReadOptions{Window: cfg.ReadWindow, Outline: cfg.ReadOutline})
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
	if !cfg.AutoVerify || cfg.VerifyCmd != "" || cfg.Root == "" || cfg.autoVerifyResolved {
		return
	}
	// Record that this session has made its one auto-verify decision before any
	// exit path below. A later TUI turn may be running against WIP, so repeating
	// derivation/preflight would both cost another suite run and let transient WIP
	// flicker the gate on or off.
	cfg.autoVerifyResolved = true
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

func recordAutoVerifyResolution(res *RunResult, cfg Config) {
	if res == nil || !cfg.autoVerifyResolved {
		return
	}
	res.autoVerifyResolved = true
	res.autoVerifyCmd = cfg.VerifyCmd
	res.autoVerifySoft = cfg.AutoVerifySoft
	res.autoVerifyProvenance = cfg.autoVerifyProvenance
	res.autoVerifyVerifyContinue = cfg.VerifyContinue
	res.autoVerifySkipVerifyBaseline = cfg.SkipVerifyBaseline
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
