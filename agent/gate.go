package agent

// The CLOSING GATES, composed (docs/specs/REVIEW-GATE.md): every path that can end a run
// Answered flows through the same three-stage gate, in evidence order —
//
//	Stage 0  test fence re-hash        (deterministic, ~free — fence.go)
//	Stage 1  VerifyCmd                 (existing, authoritative execution)
//	Stage 2  review gate               (model reviewer, ONLY if 0+1 green — review.go)
//
// gates owns the run-scoped state of stages 0 and 2 (the fence snapshot, the
// git base tree, review rounds + finding fates) and wraps the pre-existing
// verifyTermination/upgradeIfVerified logic so the two loops call ONE object
// instead of re-plumbing three gates each (the tracker lesson, review #9).
// With no fence and no reviewer configured every method degrades to the exact
// historical behavior.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/runspec"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

// Gate-stage harness-side timeouts — the harness's own bounds on external
// stages that the injected implementations may not self-bound. A stage whose
// timeout fires fails open (the existing failOpen / skipped path).
const (
	reviewTimeout    = 10 * time.Minute
	gateDiffTimeout  = 2 * time.Minute
	fenceWalkTimeout = 2 * time.Minute
	planTimeout      = 10 * time.Minute
)

// gateContext returns a context that:
//   - ignores the parent DEADLINE (an expired budget must not cancel the final gate)
//   - IS canceled when the parent is canceled explicitly (user Ctrl-C propagates)
//   - always carries the given hard timeout
//
// It is the harness-side timeout for every closing-gate external stage
// (review, diff, fence walk) and the opening plan stage.
func gateContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(parent) // drops deadline and cancellation
	ctx, cancelInner := context.WithTimeout(base, timeout)
	// Propagate only explicit user cancellation, not deadline expiry.
	// We check ctx.Err() because signal.NotifyContext (Go 1.26+) cancels with
	// a custom signalError cause, and Err() is cause-agnostic while still
	// distinguishing deadline expiry.
	stop := context.AfterFunc(parent, func() {
		if errors.Is(parent.Err(), context.Canceled) {
			cancelInner()
		}
	})
	if errors.Is(parent.Err(), context.Canceled) {
		cancelInner()
	}
	return ctx, func() {
		stop()
		cancelInner()
	}
}

// gateDeps is everything the composed closing gates consume: the resolved
// policy (by value, immutable), the runtime bindings, the run-local mutable
// verification state, and the run's evidence log. It replaces the old
// whole-Config capture (PROFILES.md §7.1).
type gateDeps struct {
	pol           runspec.PolicyValue
	rt            Runtime
	vs            *verifyState
	ev            *evidenceLog
	task          string
	root          string
	baselineCache *verifyBaselineCache
}

type gates struct {
	d          gateDeps
	runTimeout time.Duration
	fence      *fenceState
	scope      *scopeState
	review     *reviewState
	repro      *reproState

	verifyBaselineRed      bool
	verifyBaselineOut      string
	verifyBaselineMeasured bool
	verifyInfraSignature   string

	// runBaseTree is the git tree hash of the workspace at run start,
	// captured for the upgradeIfVerified empty-diff guard: a killed run
	// whose verify command passes but changed nothing (baseline was already
	// green) must not be upgraded to Answered. Empty when not captured
	// (no VerifyCmd, not a git repo, or snapshot failed — the gate
	// degrades to the historical behavior).
	runBaseTree string

	// stdBaseTree is the standing-context run-start baseline. It is captured only
	// when the feature is enabled, and may reuse runBaseTree/review/scope baselines
	// from the same run-start point.
	stdBaseTree string

	// baselineGraceUsed tracks whether the single rescue round has been
	// granted for a baseline-identical verify failure. On the FIRST
	// identical failure the gate still rejects via VerifyContinue so the
	// model sees the real red output (rescue for a fix-the-red-test task
	// where a premature "done" fingerprints identically to the baseline).
	// Only a SECOND identical failure — after a full repair round —
	// proves the gate is unsatisfiable and goes terminal.
	baselineGraceUsed bool

	// autoVerifyFeedback counts failing finish/upgrade gates for a soft,
	// harness-derived VerifyCmd. After the bounded feedback budget is spent, the
	// auto gate becomes advisory so it cannot downgrade an otherwise answered run.
	autoVerifyFeedback int

	closingVerification *VerificationRecord
	closingVerifyGen    int
}

// newGates snapshots the run-start state both closing gates need: the fence
// hashes (slice 0) and, when a Reviewer is injected, the git base tree the
// solver's diff will be taken against (slice 1a). It also measures the
// VerifyCmd baseline (pre-flight) when configured, and captures the run-start
// git tree for the upgradeIfVerified empty-diff guard.
func effectiveReviewRequired(d gateDeps) bool {
	rp := ReviewPolicy(d.pol.ReviewPolicy)
	if rp.failOpen() {
		return false
	}
	return rp.required() || d.rt.Reviewer != nil
}

func newGates(ctx context.Context, d gateDeps, runTimeout time.Duration) (*gates, error) {
	if d.rt.Obs == nil {
		d.rt.Obs = nopObserver{}
	}
	review, err := newReviewState(ctx, d)
	if err != nil {
		return nil, err
	}
	g := &gates{
		d:          d,
		runTimeout: runTimeout,
		fence:      newFenceState(ctx, d.pol.TestFence, d.rt.Sandbox, d.rt.Obs),
		scope:      newScopeState(ctx, d.pol.DiffScope, d.root, d.rt.Obs),
		review:     review,
	}
	g.measureVerifyBaseline(ctx)
	if err := g.captureRunBaseTree(ctx); err != nil {
		return nil, err
	}
	g.captureStandingBaseTree(ctx)
	return g, nil
}

// captureRunBaseTree snapshots the run-start git tree for the
// upgradeIfVerified empty-diff guard. It only runs when VerifyCmd is
// configured (the only path the guard affects). A failed snapshot leaves
// runBaseTree empty — the upgrade path degrades to the historical behavior.
func (g *gates) captureRunBaseTree(ctx context.Context) error {
	if (!g.d.pol.RequireDiff && !g.d.pol.ReproFirst && g.d.vs.Cmd == "") || g.d.root == "" {
		if g.d.pol.RequireDiff {
			return setupErr("require_diff", "require-diff needs a git workspace root to define an empty diff")
		}
		return nil
	}
	if (g.d.pol.RequireDiff || g.d.pol.ReproFirst) && !vcs.IsRepo(ctx, g.d.root) {
		if g.d.pol.ReproFirst {
			return setupErr("repro_first", "repro-first needs a git repository to validate the base tree")
		}
		return setupErr("require_diff", "require-diff needs a git repository to define an empty diff")
	}
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	tree, err := vcs.WriteTree(gctx, g.d.root)
	cancel()
	if err != nil {
		if g.d.pol.RequireDiff {
			return setupErr("require_diff", "require-diff could not capture the run baseline: "+err.Error())
		}
		return nil
	}
	g.runBaseTree = tree
	return nil
}

func (g *gates) captureStandingBaseTree(ctx context.Context) {
	if !g.d.pol.StandingContext || g.d.root == "" {
		return
	}
	if g.runBaseTree != "" {
		g.stdBaseTree = g.runBaseTree
		return
	}
	if g.scope != nil && g.scope.snapErr == nil && g.scope.base != "" {
		g.stdBaseTree = g.scope.base
		return
	}
	if g.review != nil && g.review.baseTree != "" {
		g.stdBaseTree = g.review.baseTree
		return
	}
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	tree, err := vcs.WriteTree(gctx, g.d.root)
	cancel()
	if err != nil {
		g.d.rt.Obs.Note("standing context: baseline tree snapshot failed (" + err.Error() + ") — diff section disabled")
		return
	}
	g.stdBaseTree = tree
}

func (g *gates) standingBaseTree() string {
	if g == nil {
		return ""
	}
	return g.stdBaseTree
}

// deliverableShapedGate reports whether cmd checks for artifacts the task may create.
// It recognizes grep as a standalone command word and common file-existence clauses.
func deliverableShapedGate(cmd string) bool {
	fields := strings.Fields(cmd)
	for i, field := range fields {
		if field == "grep" {
			return true
		}
		if field == "test" && i+1 < len(fields) && (fields[i+1] == "-f" || fields[i+1] == "-e") {
			return true
		}
		if field == "[" && i+1 < len(fields) && (fields[i+1] == "-f" || fields[i+1] == "-e") {
			return true
		}
	}
	return false
}

func (g *gates) measureVerifyBaseline(ctx context.Context) {
	if g.d.vs.Cmd == "" || g.d.vs.SkipVerifyBaseline {
		return
	}
	// A user cancel skips the baseline like it skips the final check.
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}

	// Sessions retain this result by complete git tree. A tree that cannot be
	// captured is intentionally uncached, preserving the historical behavior for
	// non-repositories and snapshot failures.
	cache := g.d.baselineCache
	var tree string
	if cache != nil && g.d.root != "" {
		gctx, cancel := gateContext(ctx, gateDiffTimeout)
		tree, _ = vcs.WriteTree(gctx, g.d.root)
		cancel()
		if tree != "" {
			cache.mu.Lock()
			defer cache.mu.Unlock()
			if cache.tree == tree && cache.cmd == g.d.vs.Cmd {
				g.verifyBaselineMeasured = cache.measured
				g.verifyBaselineRed = cache.red
				g.verifyBaselineOut = cache.out
				g.verifyInfraSignature = cache.infra
				return
			}
		}
	}
	// Run detached from cancellation (like verifyRun) but bounded by the
	// verify timeout — the baseline runs the SAME VerifyCmd as the closing gate.
	notifyVerifyStart(g.d.rt.Obs, g.d.vs.Cmd)
	out, err := runOp(context.WithoutCancel(ctx), g.d.rt.verifySandbox(), g.d.vs.Cmd, g.d.vs.Timeout)
	if err == nil && isRunFailure(out) {
		if isRunTimeout(out) {
			g.d.rt.Obs.Note("verify baseline: INCONCLUSIVE — " + g.d.vs.Cmd + " timed out on the untouched workspace (raise -verify-timeout)")
		} else if sig := infraFaultSignature(out); sig != "" {
			g.verifyInfraSignature = sig
			g.d.rt.Obs.Note("verify baseline: INCONCLUSIVE — " + g.d.vs.Cmd + " hit environment fault (" + sig + ") on the untouched workspace")
		} else {
			g.verifyBaselineMeasured, g.verifyBaselineRed, g.verifyBaselineOut = true, true, out
			g.d.rt.Obs.Note("verify baseline: RED — " + g.d.vs.Cmd + " already fails on the untouched workspace; if the task is not about fixing this, the gate may be unsatisfiable" + func() string {
				if deliverableShapedGate(g.d.vs.Cmd) {
					return " (the gate contains grep/test -f style deliverable checks — a red baseline may be BY DESIGN until the task's artifacts exist)"
				}
				return ""
			}())
		}
	} else if err == nil {
		g.verifyBaselineMeasured = true
	}
	if cache != nil && tree != "" {
		cache.tree, cache.cmd = tree, g.d.vs.Cmd
		cache.measured, cache.red, cache.out, cache.infra = g.verifyBaselineMeasured, g.verifyBaselineRed, g.verifyBaselineOut, g.verifyInfraSignature
	}
}

func (g *gates) applyBaseline(res *RunResult) {
	res.VerifyBaselineRed = g.verifyBaselineRed
	res.VerifyBaselineOut = g.verifyBaselineOut
	g.applyVerifyInfra(res)
}

func (g *gates) applyVerifyInfra(res *RunResult) {
	res.VerifyInfraSignature = g.verifyInfraSignature
	res.VerifyInfra = g.verifyInfraSignature != ""
}

// baselinePreamble returns an environment-preamble block warning the solver
// that the verify command already fails on the untouched workspace.  It is
// appended to the first-turn message so the solver knows the gate was red
// BEFORE any changes — it can focus on fixing the failure, or, if the task is
// unrelated, complete the task and explain the pre-existing red gate rather
// than grind against it.  Returns "" when the baseline is green or unmeasured.
func (g *gates) baselinePreamble() string {
	if !g.verifyBaselineRed {
		return ""
	}
	out := g.verifyBaselineOut
	if out == "" {
		out = "(no output)"
	} else {
		out = clip(out, 2000)
	}
	annotation := ""
	if deliverableShapedGate(g.d.vs.Cmd) {
		annotation = " (the gate contains grep/test -f style deliverable checks — a red baseline may be BY DESIGN until the task's artifacts exist)"
	}
	return fmt.Sprintf(
		"\n\n⚠️ PRE-FLIGHT VERIFY BASELINE — RED ⚠️\n"+
			"The verify command:\n"+
			"  %s\n"+
			"ALREADY fails on the UNTOUCHED workspace (before any changes):\n"+
			"%s\n\n"+
			"If your task is to fix this failure, proceed. "+
			"If your task is unrelated, the gate may be unsatisfiable%s — "+
			"complete your task, then answer explaining that the verify command "+
			"was already red before any changes rather than grinding against it.",
		g.d.vs.Cmd, out, annotation,
	)
}

// reviewReport exposes the terminal review record for RunResult.Review (nil
// when no Reviewer was configured).
func (g *gates) reviewReport() *ReviewReport { return g.review.report() }

// scopeState is the run-scoped diff-scope gate: the globs and the run-start
// git tree snapshot (for the closing-gate tree-diff check).
type scopeState struct {
	globs   []string
	base    string // run-start WriteTree hash
	snapErr error  // non-nil when the snapshot failed — closing gates fail closed as unverifiable.
}

// newScopeState snapshots the current workspace as a git tree object so the
// closing gate can detect changes outside the configured scope. nil when the
// scope is off (empty globs). A failed snapshot records a Note; because the
// armed scope cannot later be re-verified, answer/cap-rescue gates fail closed
// rather than passing or fabricating a violation.
func newScopeState(ctx context.Context, globs []string, root string, obs Observer) *scopeState {
	if len(globs) == 0 {
		return nil
	}
	s := &scopeState{globs: globs}
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	s.base, s.snapErr = vcs.WriteTree(gctx, root)
	cancel()
	if s.snapErr != nil && obs != nil {
		obs.Note("diff scope: snapshot failed (" + s.snapErr.Error() + ") — closing gates will fail closed if the run tries to finish; tool-layer refusal still active")
	}
	return s
}

type gateCheck struct {
	reason       string
	unverifiable bool
}

// scopeCheck compares the current git tree against the run-start snapshot and
// returns a structured verdict: clean, violation, or unverifiable. Snapshot and
// git errors fail closed at answer gates without being reported as violations.
func (g *gates) scopeCheck(ctx context.Context) gateCheck {
	if g.scope == nil {
		return gateCheck{}
	}
	if g.scope.snapErr != nil {
		return gateCheck{reason: "diff scope could not be re-verified: run-start snapshot failed (" + g.scope.snapErr.Error() + ")", unverifiable: true}
	}
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	cur, err := vcs.WriteTree(gctx, g.d.root)
	cancel()
	if err != nil {
		g.d.rt.Obs.Note("diff scope: closing-gate tree snapshot failed (" + err.Error() + ") — failing closed")
		return gateCheck{reason: "diff scope could not be re-verified: closing-gate tree snapshot failed (" + err.Error() + ")", unverifiable: true}
	}
	if cur == g.scope.base {
		return gateCheck{}
	}
	gctx, cancel = gateContext(ctx, gateDiffTimeout)
	changed, err := vcs.DiffTreeNames(gctx, g.d.root, g.scope.base, cur)
	cancel()
	if err != nil {
		g.d.rt.Obs.Note("diff scope: tree diff failed (" + err.Error() + ") — failing closed")
		return gateCheck{reason: "diff scope could not be re-verified: tree diff failed (" + err.Error() + ")", unverifiable: true}
	}
	var out []string
	for _, p := range changed {
		// Normalize git paths: slashes, strip leading ./
		norm := strings.TrimPrefix(path.Clean(filepath.ToSlash(p)), "./")
		if !matchesScope(g.scope.globs, norm) {
			out = append(out, norm)
		}
	}
	if len(out) == 0 {
		return gateCheck{}
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return gateCheck{reason: fmt.Sprintf("diff scope violated: %s — only paths matching (%s) may be changed",
		strings.Join(out, ", "), strings.Join(g.scope.globs, ","))}
}

// fenceCheck is stage 0 at a closing gate: re-hash the fenced files and report
// clean, violated, or unverifiable. It shares verifyRun's cancellation stance
// implicitly — hashing is read-only and cheap, so it just runs.
func (g *gates) fenceCheck(ctx context.Context) gateCheck {
	if g.fence == nil {
		return gateCheck{}
	}
	gctx, cancel := gateContext(ctx, fenceWalkTimeout)
	defer cancel()
	reason, err := g.fence.violationCheck(gctx, g.d.rt.Sandbox)
	if err != nil {
		return gateCheck{reason: "test fence could not be re-verified: " + err.Error(), unverifiable: true}
	}
	return gateCheck{reason: reason}
}

func (g *gates) verifySafety(ctx context.Context) (outcome Outcome, reason string, noContinue bool) {
	if check := g.fenceCheck(ctx); check.reason != "" {
		return Unverified, check.reason, true
	}
	if check := g.scopeCheck(ctx); check.reason != "" {
		if check.unverifiable {
			return Unverified, check.reason, true
		}
		return ScopeViolation, check.reason, false
	}
	return "", "", false
}

func (g *gates) unchangedRunTree(ctx context.Context) bool {
	if g.runBaseTree == "" {
		return false
	}
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	curTree, err := vcs.WriteTree(gctx, g.d.root)
	cancel()
	return err == nil && curTree == g.runBaseTree
}

func (g *gates) requireDiffFailure(ctx context.Context) (Outcome, string, bool) {
	if g.d.pol.RequireDiff && g.unchangedRunTree(ctx) {
		return Unverified, "require-diff: run completed with no changes", true
	}
	return "", "", false
}

func (g *gates) captureClosingVerification(out, tree string) {
	record := newVerificationRecord(g.d.vs.Cmd, out, tree)
	g.closingVerification = &record
	if g.d.ev != nil {
		g.closingVerifyGen = g.d.ev.mutGen
	}
}

func (g *gates) applyClosingVerification(res *RunResult) {
	if g.closingVerification == nil {
		return
	}
	record := *g.closingVerification
	if g.d.ev != nil && g.closingVerifyGen < g.d.ev.mutGen {
		record.Status = EvidenceDegraded
	}
	res.ClosingVerification = &record
}

func (g *gates) verificationTree(ctx context.Context) string {
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	defer cancel()
	tree, _ := vcs.WriteTree(gctx, g.d.root)
	return tree
}

func (g *gates) verifyCompletion(ctx context.Context, lastRunFailed bool) (outcome Outcome, reason string, noContinue bool) {
	if out, reason, stop := g.requireDiffFailure(ctx); reason != "" {
		return out, reason, stop
	}
	if g.runBaseTree != "" && (g.d.vs.AutoVerifySoft || (!g.d.vs.SkipVerifyBaseline && g.verifyBaselineMeasured)) {
		gctx, cancel := gateContext(ctx, gateDiffTimeout)
		curTree, err := vcs.WriteTree(gctx, g.d.root)
		cancel()
		if err == nil && curTree == g.runBaseTree {
			if g.d.vs.AutoVerifySoft && !g.verifyBaselineMeasured {
				// A soft gate arms only after its probe ran the derived suite
				// GREEN on the untouched tree (probeAutoVerify disarms on red/
				// timeout/error), so an unchanged tree needs no re-run — a
				// conversational turn must not pay a full suite at its close.
				g.d.rt.Obs.Note("auto-verify: no file changes this run — skipping soft verify gate (probe baseline was green)")
				return "", "", false
			}
			if !g.verifyBaselineRed {
				g.d.rt.Obs.Note("verify: no file changes this run — nothing to verify (baseline was green)")
				return "", "", false
			}
			reason := fmt.Sprintf("verification command %q did not pass:\n%s", g.d.vs.Cmd, g.verifyBaselineOut)
			return g.verifyCompletionFailure(reason, g.verifyBaselineOut)
		}
	}
	preVerifyTree := g.verificationTree(ctx)
	reason, verifyOut := verifyTermination(ctx, g.d.vs, g.d.rt, g.d.ev, lastRunFailed)
	if reason == "" && g.d.vs.Cmd != "" {
		g.captureClosingVerification(verifyOut, preVerifyTree)
	}
	return g.verifyCompletionFailure(reason, verifyOut)
}

func (g *gates) verifyCompletionFailure(reason, verifyOut string) (outcome Outcome, outReason string, noContinue bool) {
	if reason == "" {
		return "", "", false
	}
	if g.d.vs.AutoVerifySoft {
		g.autoVerifyFeedback++
		if g.autoVerifyFeedback > autoVerifyMaxFeedback || isInfraFault(verifyOut) {
			if sig := infraFaultSignature(verifyOut); sig != "" {
				g.verifyInfraSignature = sig
			}
			g.d.rt.Obs.Note(fmt.Sprintf("auto-derived verify `%s` did not pass — accepted anyway; supply -verify-cmd to make this authoritative", g.d.vs.Cmd))
			return "", "", false
		}
	}
	// A timed-out or infra-failed verify is INCONCLUSIVE — we could not
	// confirm success, but we also don't know anything is broken. Suppress
	// VerifyContinue: feeding "keep working: fix the code" to the model when
	// nothing is known to be broken is wrong. (Under a SOFT auto-derived gate,
	// timeouts instead consume the feedback budget above — a merely-slow suite
	// must not flip the finish green on the first attempt.)
	if !g.d.vs.AutoVerifySoft && isRunTimeout(verifyOut) {
		return Unverified, reason, true
	}
	if sig := infraFaultSignature(verifyOut); sig != "" {
		g.verifyInfraSignature = sig
		return Unverified, reason, true
	}
	if g.verifyBaselineRed && strings.Contains(reason, "did not pass:") {
		reason += " note: the verify command was ALREADY failing before any changes were made (pre-existing red baseline — the gate may be unsatisfiable)"
		if verifyOut != "" && runFingerprint(verifyOut) == runFingerprint(g.verifyBaselineOut) {
			if !g.baselineGraceUsed {
				g.baselineGraceUsed = true
				// Grant one rescue round: the first identical failure still
				// goes through VerifyContinue rejection so the model sees the
				// real red output and can do actual work. Only a second
				// identical failure — after a full round — proves the gate
				// is unsatisfiable and goes terminal.
			} else {
				noContinue = true
			}
		}
	}
	return Unverified, reason, noContinue
}

func (g *gates) verifyFinish(ctx context.Context, lastRunFailed bool, trusted bool) (outcome Outcome, reason string, noContinue bool) {
	outcome, reason, noContinue = g.verifySafety(ctx)
	if reason != "" {
		return outcome, reason, noContinue
	}
	if trusted {
		if out, reason, stop := g.requireDiffFailure(ctx); reason != "" {
			return out, reason, stop
		}
		return outcome, reason, noContinue
	}
	return g.verifyCompletion(ctx, lastRunFailed)
}

// verifyTermination is stages 0+1 for a model-initiated finish (answer /
// FinishTool): fence first — a run that edited the tests is Unverified no
// matter what the suite now says — then scope check (any changed path outside
// the configured scope is ScopeViolation, terminal), then the caller-named
// VerifyCmd (or the VerifyLastRun heuristic).
//
// It returns (outcome, reason, noContinue). When outcome is "" and reason is ""
// the gate is clean. A non-empty outcome MUST be used by the caller as
// res.Outcome — ScopeViolation is terminal regardless of VerifyContinue.
func (g *gates) verifyTermination(ctx context.Context, lastRunFailed bool) (outcome Outcome, reason string, noContinue bool) {
	return g.verifyFinish(ctx, lastRunFailed, false)
}

func appendReason(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return existing + "; " + extra
}

// upgradeIfVerified is the kill/cap/deadline/budget exit, gate-composed: the
// historical VerifyCmd upgrade now ALSO requires a clean fence and a passing
// review — a cap-rescued run must clear the same bar as an answered one (the
// gemini cap-rescue in the probe shipped a partial fix precisely because it
// didn't). Any stage failing leaves the original outcome untouched (an honest
// kill/cap, never upgraded — and never converted to Unverified, since the
// model never claimed to be done).
//
// Scope enforcement runs regardless of VerifyCmd: a shell mutation that
// escapes the scope must be caught even without a verify command configured
// (otherwise a run with DiffScope set but no VerifyCmd that ends via cap/kill
// gets no gate-level scope check — scopeCheck must never depend on a verify
// command being armed).
func (g *gates) upgradeIfVerified(ctx context.Context, res *RunResult) *RunResult {
	if check := g.scopeCheck(ctx); check.reason != "" {
		g.d.rt.Obs.Note("upgrade blocked — " + check.reason)
		if check.unverifiable {
			res.Reason = appendReason(res.Reason, "upgrade refused — "+check.reason)
			return res
		}
		res.Outcome = ScopeViolation
		res.Reason = check.reason
		return res
	}
	if out, reason, _ := g.requireDiffFailure(ctx); reason != "" {
		res.Outcome = out
		res.Reason = reason
		return res
	}
	if g.d.vs.Cmd == "" {
		return res
	}
	if check := g.fenceCheck(ctx); check.reason != "" {
		g.d.rt.Obs.Note("upgrade blocked — " + check.reason)
		res.Reason = appendReason(res.Reason, "upgrade refused — "+check.reason)
		return res
	}
	// Capture the tree BEFORE verify — the verify command itself may have
	// side effects (e.g. log files) that would otherwise make an unchanged
	// solver tree appear different from the run-start baseline.
	var preVerifyTree string
	if g.runBaseTree != "" {
		gctx, gcancel := gateContext(ctx, gateDiffTimeout)
		preVerifyTree, _ = vcs.WriteTree(gctx, g.d.root)
		gcancel()
	}
	if g.runBaseTree != "" && preVerifyTree == g.runBaseTree {
		res.Reason = fmt.Sprintf("verify passed but the run changed nothing — refusing upgrade (baseline was already green): %q passed", g.d.vs.Cmd)
		return res
	}
	out, skipped, err := verifyRun(ctx, g.d.vs, g.d.rt, g.d.ev)
	if !skipped {
		notifyVerify(g.d.rt.Obs, g.d.vs.Cmd, err == nil && !isRunFailure(out))
	}
	if skipped || err != nil {
		return res
	}
	if isRunFailure(out) {
		if sig := infraFaultSignature(out); sig != "" {
			g.verifyInfraSignature = sig
			out, skipped, err = verifyRun(ctx, g.d.vs, g.d.rt, g.d.ev)
			if !skipped {
				notifyVerify(g.d.rt.Obs, g.d.vs.Cmd, err == nil && !isRunFailure(out))
			}
			if skipped || err != nil {
				res.Reason = fmt.Sprintf("%s; verification inconclusive due to environment fault (%s)", res.Reason, sig)
				g.applyVerifyInfra(res)
				return res
			}
			if isRunFailure(out) {
				if sig2 := infraFaultSignature(out); sig2 != "" {
					g.verifyInfraSignature = sig2
					res.Reason = fmt.Sprintf("%s; verification inconclusive due to environment fault (%s)", res.Reason, sig2)
					g.applyVerifyInfra(res)
				}
				g.reviewUnverified(ctx)
				return res
			}
			g.verifyInfraSignature = ""
		} else {
			g.reviewUnverified(ctx)
			return res
		}
	}
	g.captureClosingVerification(out, preVerifyTree)
	if fb, detail := g.reproFinish(ctx); fb != "" {
		g.d.rt.Obs.Note("upgrade blocked — " + detail)
		res.Outcome = Unverified
		res.Reason = detail
		g.reviewUnverified(ctx)
		return res
	}
	// Execution is green; the review gate has the final word on the upgrade.
	// canContinue=false — the loop is over, so blockers can't be repaired.
	if _, blockReason := g.reviewFinish(ctx, false); blockReason != "" {
		g.d.rt.Obs.Note("upgrade blocked — " + blockReason)
		return res
	}
	originalOutcome := res.Outcome
	res.Reason = fmt.Sprintf("completed despite %s — %q passed", originalOutcome, g.d.vs.Cmd)
	res.RescuedFrom = originalOutcome
	res.Outcome = Answered
	if res.Answer == "" {
		res.Answer = fmt.Sprintf("task verified complete (%q passed)", g.d.vs.Cmd)
	}
	return res
}

// reviewFinish is stage 2 for a finish whose fence+verify already passed. It
// returns exactly one of:
//
//	feedback != ""    blocking OR advisory findings, AND repair budget
//	                  remains — feed the observation back to the solver and
//	                  keep looping (1d);
//	blockReason != "" blocking findings and no budget — the caller records
//	                  Unverified (exit 2) with the findings in the report;
//	both ""           pass — no reviewer / skipped / no blockers (advisories
//	                  alone never block; with no budget left they pass).
func (g *gates) reviewFinish(ctx context.Context, canContinue bool) (feedback, blockReason string) {
	rv := g.review
	if rv == nil {
		return "", ""
	}
	if rv.skip != "" {
		if effectiveReviewRequired(g.d) {
			return "", fmt.Sprintf("review required but review was skipped: %s", rv.skip)
		}
		return "", ""
	}
	// A user cancel skips the reviewer like it skips VerifyCmd (verifyRun's
	// policy): the user asked us to stop, so we spend nothing more. The finish
	// stays un-reviewed, honestly — pass, don't block.
	if errors.Is(ctx.Err(), context.Canceled) {
		rv.status = ReviewCanceled
		return "", ""
	}
	// A deadline expiry must not clip the final gate (review #3's policy);
	// gateContext preserves that AND adds a harness-side timeout while still
	// propagating explicit user cancellation.
	gctx, gcancel := gateContext(ctx, reviewTimeout)
	defer gcancel()

	diff, err := rv.captureDiff(gctx)
	if err != nil {
		g.failOpen(classifyReviewErr(gctx, err), "review error (gate fails open): "+err.Error())
		return "", g.reviewRequiredBlockReason()
	}
	if g.d.ev != nil {
		g.d.ev.reviewedGen = g.d.ev.mutGen
		g.d.ev.reviewReached = true
	}
	if strings.TrimSpace(diff) == "" {
		g.setReviewSemanticStatus(ReviewClean)
		g.d.rt.Obs.Note("review: no changes to review — skipping")
		return "", ""
	}

	rv.resolvePending(FateRepaired) // last round's fed-back blockers were re-reviewed by reaching here.
	rv.rounds++
	if ro := reviewObserver(g.d.rt.Obs); ro != nil {
		model := rv.model
		if model == "" {
			if n, ok := g.d.rt.Reviewer.(ReviewerModelNamer); ok {
				model = n.ReviewerModel()
			}
		}
		ro.ReviewStart(rv.rounds, model)
	}
	verdict, err := g.d.rt.Reviewer.Review(gctx, ReviewInput{
		Task:       g.d.task,
		Diff:       diff,
		Root:       g.d.root,
		Signals:    substanceSignals(diff),
		SessionKey: rv.sessionKey,
		Round:      rv.rounds,
	})
	if verdict != nil {
		rv.usage = addUsage(rv.usage, verdict.Usage)
		g.d.rt.Spend.Add(verdict.Model, verdict.Usage) // charge each reviewer round's dollars
		if rv.model == "" {
			rv.model = verdict.Model
		}
		rv.summaries = append(rv.summaries, verdict.Summary)
		if verdict.RunID != "" {
			rv.runIDs = append(rv.runIDs, verdict.RunID)
		}
	}
	if err != nil {
		// The reviewer is advisory-blocking, not authoritative: execution already
		// passed, so a reviewer INFRA failure must not flip a verified run to
		// Unverified unless the caller explicitly requires a completed review.
		g.failOpen(classifyReviewErr(gctx, err), "review error (gate fails open): "+err.Error())
		return "", g.reviewRequiredBlockReason()
	}

	// Repro solicitation (slice 2c): blocker findings with Confidence >= 7
	// that arrived WITHOUT a repro command are sent back to the reviewer in
	// one batched follow-up call. The reviewer can supply runnable repro_cmds
	// or explicit no_repro_reason strings. This closes the gap where
	// high-confidence blockers expired unverified because the reviewer never
	// provided a command — the measured run had 4/4 conf-8-9 blockers
	// arrive without repro_cmd, so zero were confirmed.
	if solicitor, ok := g.d.rt.Reviewer.(ReproSolicitor); ok {
		var eligible []ReviewFinding
		for _, f := range verdict.Findings {
			if strings.ToLower(strings.TrimSpace(f.Severity)) == "blocker" &&
				f.Confidence >= 7 &&
				strings.TrimSpace(f.ReproCmd) == "" {
				eligible = append(eligible, f)
			}
		}
		if len(eligible) > 0 {
			merged, solicitUsage, serr := solicitor.SolicitRepro(gctx, ReviewInput{
				Task:       g.d.task,
				Root:       g.d.root,
				SessionKey: rv.sessionKey,
				Round:      rv.rounds,
			}, eligible)
			rv.usage = addUsage(rv.usage, solicitUsage)
			g.d.rt.Spend.Add(verdict.Model, solicitUsage) // charge reviewer follow-up dollars
			if serr != nil {
				// Fail-open: solicitation errors leave findings as-is, but record
				// infrastructure honesty because required review depends on this
				// structured reviewer output too.
				status := classifyReviewErr(gctx, serr)
				if status != ReviewCanceled {
					rv.status = status
				}
				g.d.rt.Obs.Note("review: repro solicitation failed (findings stay unconfirmed): " + serr.Error())
			} else {
				// Merge returned repro_cmds/no_repro_reasons back into
				// the verdict findings in place (match by file+quote).
				for i := range verdict.Findings {
					for _, m := range merged {
						if strings.TrimSpace(verdict.Findings[i].File) == strings.TrimSpace(m.File) &&
							strings.TrimSpace(verdict.Findings[i].Quote) == strings.TrimSpace(m.Quote) {
							if m.ReproCmd != "" {
								verdict.Findings[i].ReproCmd = m.ReproCmd
							}
							if m.NoReproReason != "" {
								verdict.Findings[i].NoReproReason = m.NoReproReason
							}
							break
						}
					}
				}
			}
		}
	}

	blocking := 0
	var blockers, advisories []ReviewedFinding
	reproBudget := reviewReproCap
	for _, f := range verdict.Findings {
		if ro := reviewObserver(g.d.rt.Obs); ro != nil {
			ro.ReviewFinding(f)
		}
		rf := g.classify(gctx, f, &reproBudget)
		rf.Round = rv.rounds
		if rf.ID == "" {
			rf.ID = fmt.Sprintf("r%d-f%d", rv.rounds, len(rv.findings)+1)
		}
		rv.findings = append(rv.findings, rf)
		switch rf.Fate {
		case "": // pending = blocking.
			blocking++
			rv.pending = append(rv.pending, len(rv.findings)-1)
			blockers = append(blockers, rf)
		case FateAdvised:
			advisories = append(advisories, rf)
		}
	}
	if ro := reviewObserver(g.d.rt.Obs); ro != nil {
		ro.ReviewVerdict(blocking, rv.rounds, verdict.Summary)
	}
	// Early-stop: a CONFIRMED blocker whose File+Quote recurred unchanged
	// after a repair round proves the solver cannot fix it — stop now
	// instead of burning every remaining round on it.
	if curIdx, prevIdx, ok := rv.findRecurringConfirmedBlocker(); ok {
		rv.status = ReviewBlocked
		rv.findings[prevIdx].Fate = FateExpired // correct the earlier Repaired mislabel.
		rv.blocked = true
		rv.resolvePending(FateExpired)
		cur := rv.findings[curIdx]
		g.d.rt.Obs.Note(fmt.Sprintf("review: confirmed blocker recurred unresolved after a repair round — stopping early: %s: %s",
			cur.File, oneLine(cur.Quote)))
		return "", fmt.Sprintf("confirmed review blocker recurred unresolved after repair round %d", rv.rounds)
	}
	notifyNote(g.d.rt.Obs, NoteReviewRound, fmt.Sprintf("review: round %d/%d — %d finding(s), %d blocking, %d advisory", rv.rounds, rv.maxRounds, len(verdict.Findings), blocking, len(advisories)))

	if blocking == 0 && len(advisories) == 0 {
		g.setReviewSemanticStatus(ReviewClean)
		return "", g.reviewRequiredBlockReason()
	}
	if canContinue && rv.rounds < rv.maxRounds {
		g.setReviewSemanticStatus(ReviewBlocked)
		if blocking == 0 {
			g.setReviewSemanticStatus(ReviewAdvisory)
		}
		return reviewFeedback(blockers, advisories), ""
	}
	if blocking == 0 {
		// Only advisories stand and the repair budget is gone: they were
		// hearsay by construction (unconfirmed, sub-threshold) — pass.
		g.setReviewSemanticStatus(ReviewAdvisory)
		return "", g.reviewRequiredBlockReason()
	}
	rv.status = ReviewBlocked
	rv.blocked = true
	rv.resolvePending(FateExpired)
	return "", fmt.Sprintf("review blockers remain after %d round(s)", rv.rounds)
}

// reviewUnverified runs a single advisory salvage review for an Unverified
// exit. It records reviewer signal only; callers keep their existing Outcome,
// Reason, and exit-code class regardless of the verdict.
func (g *gates) reviewUnverified(ctx context.Context) {
	if !g.d.pol.ReviewUnverified {
		return
	}
	rv := g.review
	if rv == nil || rv.skip != "" || rv.rounds > 0 || len(rv.findings) > 0 {
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	gctx, gcancel := gateContext(ctx, gateDiffTimeout)
	diff, err := rv.captureDiff(gctx)
	gcancel()
	if g.d.ev != nil {
		g.d.ev.reviewedGen = g.d.ev.mutGen
		g.d.ev.reviewReached = true
	}
	if err != nil {
		if rv.skip == "" && rv.status == "" {
			rv.skip = "review skipped: could not capture diff for unverified advisory review: " + err.Error()
			rv.status = ReviewUnavailable
		}
		return
	}
	if strings.TrimSpace(diff) == "" {
		if rv.skip == "" && rv.status == "" {
			rv.skip = "review skipped: no changes to review on unverified exit"
		}
		return
	}
	rv.salvage = true
	_, _ = g.reviewFinish(ctx, false)
}

// failOpen records a review-infrastructure failure without blocking: note it,
// and if the gate has produced no findings yet, persist the reason as the
// report's skip field so the telemetry says WHY it contributed nothing.
func (g *gates) failOpen(status ReviewStatus, msg string) {
	g.d.rt.Obs.Note(msg)
	g.review.status = status
	if len(g.review.findings) == 0 && g.review.skip == "" {
		g.review.skip = msg
	}
}

func classifyReviewErr(ctx context.Context, err error) ReviewStatus {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return ReviewCanceled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ReviewTimeout
	case errors.Is(err, ErrReviewParse):
		return ReviewParseError
	default:
		return ReviewUnavailable
	}
}

func (g *gates) setReviewSemanticStatus(status ReviewStatus) {
	if g.review == nil || isReviewInfrastructureStatus(g.review.status) {
		return
	}
	g.review.status = status
}

func isReviewInfrastructureStatus(status ReviewStatus) bool {
	switch status {
	case ReviewUnavailable, ReviewParseError, ReviewTimeout:
		return true
	default:
		return false
	}
}

func (g *gates) reviewRequiredBlockReason() string {
	if g.review == nil || !effectiveReviewRequired(g.d) || !isReviewInfrastructureStatus(g.review.status) {
		return ""
	}
	return fmt.Sprintf("review required but review status is %s", g.review.status)
}

// captureDiff writes the CURRENT working tree as a second temp-index tree and
// diffs it against the run-start base — tracked and untracked files included,
// .git and gitignored noise excluded, nothing about the user's index touched.
func (rv *reviewState) captureDiff(ctx context.Context) (string, error) {
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	defer cancel()
	cur, err := vcs.WriteTree(gctx, rv.root)
	if err != nil {
		return "", err
	}
	if cur == rv.baseTree {
		return "", nil
	}
	return vcs.DiffTrees(gctx, rv.root, rv.baseTree, cur)
}

// classify runs the deterministic validation ladder on one finding (1c + slice
// 2), in evidence order:
//
//  1. verbatim-quote re-grounding: the quote must appear in the named
//     post-patch file, or the finding is DROPPED (PR-Agent's score-0 rule —
//     the measured anti-hallucination filter);
//  2. severity: a "note" (or anything unrecognized) never blocks;
//  3. executable escalation: a blocker carrying a repro command has it RUN on
//     the verify sandbox — failing output CONFIRMS the finding (blocks
//     regardless of confidence, the output travels in the feedback). A
//     passing repro REFUTES a finding below reviewBlockConfidence
//     (downgraded, fate refuted), but a high-confidence finding whose repro
//     passes is only downgraded to ADVISORY (fate advised) — a clean run of
//     a race reproducer is weak evidence. Capped per round; a repro that
//     mutates the workspace is rejected and its damage restored;
//  4. the confidence ladder: an unconfirmed blocker below
//     reviewBlockConfidence is ADVISORY (fed back, never blocks) down to
//     reviewAdviseConfidence, and a silent note below that.
//
// Fate "" means BLOCKING (pending repair/expiry — resolved by later rounds).
func (g *gates) classify(ctx context.Context, f ReviewFinding, reproBudget *int) ReviewedFinding {
	rf := ReviewedFinding{ReviewFinding: f}
	quote := strings.TrimSpace(f.Quote)
	if quote == "" {
		rf.Fate, rf.DropWhy = FateDropped, "empty quote"
		return rf
	}
	file := strings.TrimSpace(f.File)
	if file != "" && !strings.EqualFold(file, "n/a") {
		data, _, err := readBounded(ctx, g.d.rt.Sandbox, fenceRelPath(g.review.alias, f.File))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			rf.Fate, rf.DropWhy = FateDropped, fmt.Sprintf("quoted file unreadable: %v", err)
			return rf
		}
		if err == nil && !strings.Contains(string(data), quote) {
			rf.Fate, rf.DropWhy = FateDropped, "quote does not appear verbatim in "+f.File
			return rf
		}
	}
	if strings.ToLower(strings.TrimSpace(f.Severity)) != "blocker" {
		rf.Fate = FateNote
		return rf
	}
	if cmd := strings.TrimSpace(f.ReproCmd); cmd != "" && *reproBudget > 0 {
		*reproBudget--
		if done := g.escalate(ctx, cmd, &rf); done {
			return rf
		}
	}
	if f.Confidence < reviewBlockConfidence {
		// Between the two thresholds the finding is ADVISORY: fed back for a
		// repair round (the solver hears it), never blocking (it can't raise
		// the false-block rate). Below the advisory floor it stays a silent
		// note — low-confidence chatter isn't worth a billed round.
		if f.Confidence >= reviewAdviseConfidence {
			rf.Fate = FateAdvised
			return rf
		}
		rf.Fate = FateNote
		return rf
	}
	return rf // blocking (fate pending).
}

// escalate runs a finding's repro command on the verify sandbox (same context
// policy as verifyRun: detached from cancellation, bounded by the run
// timeout). Returns true when execution SETTLED the finding — confirmed
// (repro failed), refuted (repro passed, low confidence), advised (repro
// passed, high confidence), or workspace-rejected. False means execution gave no
// signal (the command couldn't start) and the prose ladder continues.
func (g *gates) escalate(ctx context.Context, cmd string, rf *ReviewedFinding) bool {
	snapCtx, cancel := gateContext(ctx, gateDiffTimeout)
	preTree, err := vcs.WriteTree(snapCtx, g.d.root)
	cancel()
	if err != nil {
		g.d.rt.Obs.Note("review: repro skipped because the workspace could not be snapshotted (" + err.Error() + ") — falling back to the confidence gate")
		return false
	}

	out, err := runOp(ctx, g.d.rt.verifySandbox(), cmd, g.runTimeout)
	// A repro must never mutate the workspace (the reviewer's brief says new
	// files go under /tmp). Bracket it with temp-index git trees so unfenced
	// tracked/untracked changes are caught without touching the user's real
	// index. Keep the fence check too: it covers configured ignored paths that
	// WriteTree intentionally excludes.
	var fencedChanged []string
	if g.fence != nil {
		fencedChanged = g.fence.drift(ctx, g.d.rt.Sandbox)
	}
	snapCtx, cancel = gateContext(ctx, gateDiffTimeout)
	postTree, snapErr := vcs.WriteTree(snapCtx, g.d.root)
	cancel()
	if snapErr != nil || postTree != preTree || len(fencedChanged) > 0 {
		var changed []string
		if snapErr == nil && postTree != preTree {
			snapCtx, cancel = gateContext(ctx, gateDiffTimeout)
			changed, _ = vcs.DiffTreeNames(snapCtx, g.d.root, preTree, postTree)
			cancel()
		}
		if len(fencedChanged) > 0 {
			if rerr := g.fence.restore(ctx, g.d.rt.Sandbox, fencedChanged); rerr != nil {
				g.d.rt.Obs.Note("review: repro mutated fenced paths and fence restore failed: " + rerr.Error())
			}
		}
		if snapErr == nil && postTree != preTree {
			snapCtx, cancel = gateContext(ctx, gateDiffTimeout)
			if rerr := vcs.RestoreTree(snapCtx, g.d.root, preTree); rerr != nil {
				g.d.rt.Obs.Note("review: repro modified the workspace and restore failed: " + rerr.Error())
			}
			cancel()
		} else if snapErr != nil {
			snapCtx, cancel = gateContext(ctx, gateDiffTimeout)
			if rerr := vcs.RestoreTree(snapCtx, g.d.root, preTree); rerr != nil {
				g.d.rt.Obs.Note("review: repro left the workspace unsnapshottable and restore failed: " + rerr.Error())
			}
			cancel()
		}
		if len(fencedChanged) > 0 {
			rf.Fate, rf.DropWhy = FateDropped, "repro command modified fenced paths: "+strings.Join(fencedChanged, ", ")
		} else if snapErr != nil {
			rf.Fate, rf.DropWhy = FateDropped, "repro command left the workspace unsnapshottable: "+snapErr.Error()
		} else {
			rf.Fate, rf.DropWhy = FateDropped, "repro command modified the workspace: "+pathSummary(changed)
		}
		g.d.rt.Obs.Note("review: " + rf.DropWhy)
		return true
	}
	if err != nil {
		g.d.rt.Obs.Note("review: repro could not run (" + err.Error() + ") — falling back to the confidence gate")
		return false
	}
	rf.ReproOut = clip(out, runStreamCap)
	if isRunFailure(out) {
		rf.Confirmed = true // execution evidence beats prose — blocks regardless of confidence.
		return true
	}
	if rf.Confidence < reviewBlockConfidence {
		rf.Fate = FateRefuted // the claim was refuted by execution: a note, recorded as such.
	} else {
		rf.Fate = FateAdvised // high-confidence clean run: inconclusive, downgraded to advisory.
	}
	return true
}

func pathSummary(paths []string) string {
	if len(paths) == 0 {
		return "tree hash changed"
	}
	const max = 5
	if len(paths) <= max {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%d paths (%s, …)", len(paths), strings.Join(paths[:max], ", "))
}

// reviewFeedback renders blocking findings as the repair observation (1d) —
// the exact VerifyContinue pattern: the real defect, then "keep working".
func reviewFeedback(blockers, advisories []ReviewedFinding) string {
	var b strings.Builder
	for _, f := range blockers {
		fmt.Fprintf(&b, "[review] a reviewer found a blocking defect: %s (%s: %q).\n", f.FailureScenario, f.File, f.Quote)
		if f.Confirmed {
			fmt.Fprintf(&b, "The defect was CONFIRMED by executing `%s`, which failed:\n%s\n", f.ReproCmd, f.ReproOut)
		}
	}
	// Advisories are worded as likely-but-unproven so the solver repairs what
	// is real instead of treating every line as a command — the reviewer's
	// confidence was below the blocking bar, and a wrong "fix" to a correct
	// patch is the Self-Refine failure mode the round cap exists for.
	for _, f := range advisories {
		fmt.Fprintf(&b, "[review] a reviewer flagged a LIKELY defect (advisory — this will not block): %s (%s: %q).\n", f.FailureScenario, f.File, f.Quote)
	}
	b.WriteString("Fix what is real without breaking the verified tests, verifying an advisory against the code before acting on it; do not argue with the review in prose.")
	return b.String()
}

// ---- standalone gate entry (slice 3: gate-only evaluation) ----

// Gate is the closing gate detached from the agent loop, for gate-only
// evaluation (REVIEW-GATE-PLAN §3/§5.1): construct it over a prepared
// workspace (snapshotting the fence and the diff base), apply a candidate
// patch by any external means, then Check — the same fence → verify → review
// ladder a live run's finish flows through, without a solver.
type Gate struct{ g *gates }

// GateReport is one standalone gate verdict. Blocked is the headline: a banked
// BAD patch must come back Blocked (CAUGHT), a good one must not (PASSED).
type GateReport struct {
	Blocked        bool          `json:"blocked"`
	FenceViolation string        `json:"fence_violation,omitempty"`
	VerifyReason   string        `json:"verify_reason,omitempty"`
	Review         *ReviewReport `json:"review,omitempty"`
}

// NewGate snapshots the workspace's gate baseline. Runtime needs Sandbox (and
// a Reviewer when the review stage should run); the spec carries whichever
// gate knobs apply (TestFence, VerifyCmd, RunTimeout); Model is not used —
// there is no solver. A setup failure (e.g. reviewer armed on a tree the
// review gate cannot baseline) is returned, never swallowed: a silently
// review-less verdict is exactly the fail-open the 2026-07-07 fail-closed
// change exists to prevent.
func NewGate(ctx context.Context, spec runspec.ResolvedSpec, rt Runtime, content Content) (*Gate, error) {
	if err := spec.Complete(); err != nil {
		return nil, setupErr("invalid_config", err.Error())
	}
	if rt.Obs == nil {
		rt.Obs = nopObserver{}
	}
	pol := spec.Policy()
	vs := newVerifyState(pol)
	// The standalone gate judges a FINISHED tree — there is no "before any changes" to baseline.
	vs.SkipVerifyBaseline = true
	g, err := newGates(ctx, gateDeps{pol: pol, rt: rt, vs: vs, task: content.Task, root: content.Root}, pol.RunTimeout)
	if err != nil {
		return nil, err
	}
	return &Gate{g: g}, nil
}

// Check runs the composed closing gate once and reports every stage's verdict.
// Stage order and short-circuiting mirror the live loop: the reviewer runs
// ONLY when the fence and VerifyCmd are green (execution-first).
func (t *Gate) Check(ctx context.Context) GateReport {
	rep := GateReport{}
	if check := t.g.fenceCheck(ctx); check.reason != "" {
		rep.FenceViolation = check.reason
		rep.Blocked = true
	} else if rep.VerifyReason, _ = verifyTermination(ctx, t.g.d.vs, t.g.d.rt, t.g.d.ev, false); rep.VerifyReason != "" {
		rep.Blocked = true
	} else if _, blockReason := t.g.reviewFinish(ctx, false); blockReason != "" {
		rep.Blocked = true
	}
	rep.Review = t.g.reviewReport()
	return rep
}
