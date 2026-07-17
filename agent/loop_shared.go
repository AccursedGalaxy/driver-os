package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/runspec"
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

func wrapTools(pol runspec.PolicyValue, rt Runtime, root string, runTimeout time.Duration) map[string]Tool {
	tools := rt.Tools
	if tools == nil {
		tools = DefaultTools(rt.Sandbox, runTimeout, ReadOptions{Window: pol.ReadWindow, Outline: pol.ReadOutline})
	}
	// (REVIEW-GATE slice 0) The test fence wraps the mutation tools BEFORE the
	// system prompt is built from them, and the closing gates snapshot their
	// run-start state (fence hashes + the review diff base). Empty fence + nil
	// Reviewer ⇒ both are inert and behavior is byte-identical.
	// Diff-scope wraps FIRST, test-fence LAST, so the fence wins for fenced
	// in-scope paths (the refusal names whichever mechanism refused).
	tools = applyDiffScope(tools, pol.DiffScope, rt.Sandbox)
	tools = applyTestFence(tools, pol.TestFence, rt.Sandbox, pol.ReproFirst, root)
	return tools
}

type autoVerifyResolution struct {
	cmd, provenance string
	derived         bool
	disarmNote      string
}

// probeAutoVerify derives and preflights an automatic verify command without
// changing any state or reporting observations. It is safe to run before a
// turn because its result is applied to run-local verifyState later.
func probeAutoVerify(ctx context.Context, verifySB sandbox.Sandbox, root string) autoVerifyResolution {
	cmd, prov := deriveVerifyCmd(ctx, verifySB, root)
	if cmd == "" {
		return autoVerifyResolution{}
	}
	res := autoVerifyResolution{cmd: cmd, provenance: prov, derived: true}
	out, err := runOp(context.WithoutCancel(ctx), verifySB, cmd, autoVerifyBaselineTimeout)
	if err != nil {
		res.disarmNote = fmt.Sprintf("auto-verify: disarmed `%s` because the baseline could not run: %v", cmd, err)
		return res
	}
	if isRunFailure(out) {
		if isRunTimeout(out) {
			res.disarmNote = fmt.Sprintf("auto-verify: disarmed `%s` because the baseline timed out after %s", cmd, autoVerifyBaselineTimeout)
		} else {
			res.disarmNote = fmt.Sprintf("auto-verify: disarmed `%s` because it is already red on the untouched workspace", cmd)
		}
		return res
	}
	return res
}

// applyAutoVerifyResolution applies a completed probe to the run's mutable
// verification state — NEVER to the spec (PROFILES.md §7.3: runtime-derived
// values are not config). emit controls the observations: background probes
// must report only when their result is consumed by a turn.
func applyAutoVerifyResolution(vs *verifyState, res autoVerifyResolution, obs Observer, emit bool) {
	vs.resolved = true
	if !res.derived {
		return
	}
	if emit && obs != nil {
		obs.Note(fmt.Sprintf("verify gate auto-derived from %s: `%s`", res.provenance, res.cmd))
	}
	vs.SkipVerifyBaseline = true
	if res.disarmNote != "" {
		vs.recordResolution("pre-run", "verify_cmd", res.cmd, res.provenance, "verify-gate", true, res.disarmNote)
		if emit && obs != nil {
			obs.Note(res.disarmNote)
		}
		return
	}
	vs.Cmd = res.cmd
	vs.AutoVerifySoft = true
	vs.VerifyContinue = true
	vs.provenance = res.provenance
	vs.recordResolution("pre-run", "verify_cmd", res.cmd, res.provenance, "verify-gate", false, "auto-derived soft verify gate")
	if emit && obs != nil {
		obs.Note(fmt.Sprintf("auto-verify: armed soft verify gate `%s` (derived from %s)", res.cmd, res.provenance))
	}
}

func resolveAutoVerify(ctx context.Context, vs *verifyState, pol runspec.PolicyValue, rt Runtime, root string) {
	if !vs.AutoVerify || vs.Cmd != "" || root == "" || vs.resolved {
		return
	}
	// Record that this session has made its one auto-verify decision before any
	// exit path below. A later TUI turn may be running against WIP, so repeating
	// derivation/preflight would both cost another suite run and let transient WIP
	// flicker the gate on or off.
	vs.resolved = true
	if pol.MinIsolation > sandbox.IsolationNone {
		rt.obs().Note("auto-verify: off for untrusted isolation; supply -verify-cmd to arm an explicit gate")
		return
	}
	applyAutoVerifyResolution(vs, probeAutoVerify(ctx, rt.verifySandbox(), root), rt.obs(), true)
}

func recordAutoVerifyResolution(res *RunResult, vs *verifyState) {
	if res == nil || !vs.resolved {
		return
	}
	res.autoVerifyResolved = true
	res.autoVerifyCmd = vs.Cmd
	res.autoVerifySoft = vs.AutoVerifySoft
	res.autoVerifyProvenance = vs.provenance
	res.autoVerifyVerifyContinue = vs.VerifyContinue
	res.autoVerifySkipVerifyBaseline = vs.SkipVerifyBaseline
}

func redBaselineRefusal(vs *verifyState, gs *gates) *RunResult {
	// AbortOnRedBaseline: if the baseline is red and the caller wants us to stop,
	// return immediately before any model call — the gate is unsatisfiable at base.
	if vs.AbortOnRedBaseline && gs.verifyBaselineRed {
		res := &RunResult{}
		res.Outcome = Unverified
		res.Reason = fmt.Sprintf(
			"refused to run: the verify command %q is ALREADY failing on the untouched workspace — "+
				"the gate is unsatisfiable at base (-verify-baseline=abort caused this refusal)",
			vs.Cmd,
		)
		res.Iterations = 0
		return res
	}
	return nil
}

func classifyGenerateErr(ctx, loopCtx context.Context, maxWallClock time.Duration, err error) (outcome Outcome, reason string, returnErr error, ok bool) {
	// A deadline hit mid-Generate is the wall-clock budget, not a transport fault.
	if maxWallClock > 0 && loopCtx.Err() == context.DeadlineExceeded {
		return HitDeadline, fmt.Sprintf("hit wall-clock budget (%s) mid-turn", maxWallClock), nil, true
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
