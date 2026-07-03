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
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/vcs"
)

type gates struct {
	cfg        Config
	runTimeout time.Duration
	fence      *fenceState
	review     *reviewState

	verifyBaselineRed bool
	verifyBaselineOut string
}

// newGates snapshots the run-start state both closing gates need: the fence
// hashes (slice 0) and, when a Reviewer is injected, the git base tree the
// solver's diff will be taken against (slice 1a). It also measures the
// VerifyCmd baseline (pre-flight) when configured.
func newGates(ctx context.Context, cfg Config, runTimeout time.Duration) *gates {
	g := &gates{
		cfg:        cfg,
		runTimeout: runTimeout,
		fence:      newFenceState(ctx, cfg),
		review:     newReviewState(ctx, cfg),
	}
	g.measureVerifyBaseline(ctx)
	return g
}

func (g *gates) measureVerifyBaseline(ctx context.Context) {
	if g.cfg.VerifyCmd == "" || g.cfg.SkipVerifyBaseline {
		return
	}
	// A user cancel skips the baseline like it skips the final check.
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	// Run detached from cancellation (like verifyRun) but bounded by the sandbox timeout.
	out, err := runOp(context.WithoutCancel(ctx), g.cfg.verifySandbox(), g.cfg.VerifyCmd, g.runTimeout)
	if err != nil {
		return // couldn't even start it — no signal.
	}
	if isRunFailure(out) {
		g.verifyBaselineRed = true
		g.verifyBaselineOut = out
		g.cfg.Obs.Note("verify baseline: RED — " + g.cfg.VerifyCmd + " already fails on the untouched workspace; if the task is not about fixing this, the gate may be unsatisfiable")
	}
}

func (g *gates) applyBaseline(res *RunResult) {
	res.VerifyBaselineRed = g.verifyBaselineRed
	res.VerifyBaselineOut = g.verifyBaselineOut
}

// reviewReport exposes the terminal review record for RunResult.Review (nil
// when no Reviewer was configured).
func (g *gates) reviewReport() *ReviewReport { return g.review.report() }

// fenceCheck is stage 0 at a closing gate: re-hash the fenced files and name
// any drift ("" = clean / fence off). It shares verifyRun's cancellation
// stance implicitly — hashing is read-only and cheap, so it just runs.
func (g *gates) fenceCheck(ctx context.Context) string {
	if g.fence == nil {
		return ""
	}
	return g.fence.violation(context.WithoutCancel(ctx), g.cfg.Sandbox)
}

// verifyTermination is stages 0+1 for a model-initiated finish (answer /
// FinishTool): fence first — a run that edited the tests is Unverified no
// matter what the suite now says — then the caller-named VerifyCmd (or the
// VerifyLastRun heuristic). Non-empty reason ⇒ the finish does not hold.
func (g *gates) verifyTermination(ctx context.Context, lastRunFailed bool) string {
	if reason := g.fenceCheck(ctx); reason != "" {
		return reason
	}
	reason := verifyTermination(ctx, g.cfg, lastRunFailed, g.runTimeout)
	if reason != "" && g.verifyBaselineRed && strings.Contains(reason, "did not pass:") {
		reason += " note: the verify command was ALREADY failing before any changes were made (pre-existing red baseline — the gate may be unsatisfiable)"
	}
	return reason
}

// upgradeIfVerified is the kill/cap/deadline/budget exit, gate-composed: the
// historical VerifyCmd upgrade now ALSO requires a clean fence and a passing
// review — a cap-rescued run must clear the same bar as an answered one (the
// gemini cap-rescue in the probe shipped a partial fix precisely because it
// didn't). Any stage failing leaves the original outcome untouched (an honest
// kill/cap, never upgraded — and never converted to Unverified, since the
// model never claimed to be done).
func (g *gates) upgradeIfVerified(ctx context.Context, res *RunResult) *RunResult {
	if g.cfg.VerifyCmd == "" {
		return res
	}
	if reason := g.fenceCheck(ctx); reason != "" {
		g.cfg.Obs.Note("upgrade blocked — " + reason)
		return res
	}
	out, skipped, err := verifyRun(ctx, g.cfg, g.runTimeout)
	if !skipped {
		notifyVerify(g.cfg.Obs, g.cfg.VerifyCmd, err == nil && !isRunFailure(out))
	}
	if skipped || err != nil || isRunFailure(out) {
		return res
	}
	// Execution is green; the review gate has the final word on the upgrade.
	// canContinue=false — the loop is over, so blockers can't be repaired.
	if _, blockReason := g.reviewFinish(ctx, false); blockReason != "" {
		g.cfg.Obs.Note("upgrade blocked — " + blockReason)
		return res
	}
	res.Reason = fmt.Sprintf("completed despite %s — %q passed", res.Outcome, g.cfg.VerifyCmd)
	res.Outcome = Answered
	if res.Answer == "" {
		res.Answer = fmt.Sprintf("task verified complete (%q passed)", g.cfg.VerifyCmd)
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
	if rv == nil || rv.skip != "" {
		return "", ""
	}
	// A user cancel skips the reviewer like it skips VerifyCmd (verifyRun's
	// policy): the user asked us to stop, so we spend nothing more. The finish
	// stays un-reviewed, honestly — pass, don't block.
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", ""
	}
	gctx := context.WithoutCancel(ctx) // a deadline expiry must not clip the final gate (review #3's policy).

	diff, err := rv.captureDiff(gctx)
	if err != nil {
		g.failOpen("review error (gate fails open): " + err.Error())
		return "", ""
	}
	if strings.TrimSpace(diff) == "" {
		g.cfg.Obs.Note("review: no changes to review — skipping")
		return "", ""
	}

	rv.resolvePending(FateRepaired) // last round's fed-back blockers were re-reviewed by reaching here.
	rv.rounds++
	if ro := reviewObserver(g.cfg.Obs); ro != nil {
		ro.ReviewStart(rv.rounds)
	}
	verdict, err := g.cfg.Reviewer.Review(gctx, ReviewInput{
		Task:       g.cfg.Task,
		Diff:       diff,
		Root:       g.cfg.Root,
		Signals:    substanceSignals(diff),
		SessionKey: rv.sessionKey,
		Round:      rv.rounds,
	})
	if verdict != nil {
		rv.usage = addUsage(rv.usage, verdict.Usage)
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
		// Unverified. Fail open, loudly, and record it.
		g.failOpen("review error (gate fails open): " + err.Error())
		return "", ""
	}

	blocking := 0
	var blockers, advisories []ReviewedFinding
	reproBudget := reviewReproCap
	for _, f := range verdict.Findings {
		if ro := reviewObserver(g.cfg.Obs); ro != nil {
			ro.ReviewFinding(f)
		}
		rf := g.classify(gctx, f, &reproBudget)
		rf.Round = rv.rounds
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
	g.cfg.Obs.Note(fmt.Sprintf("review: round %d/%d — %d finding(s), %d blocking, %d advisory", rv.rounds, rv.maxRounds, len(verdict.Findings), blocking, len(advisories)))
	if ro := reviewObserver(g.cfg.Obs); ro != nil {
		ro.ReviewVerdict(blocking, rv.rounds, verdict.Summary)
	}

	if blocking == 0 && len(advisories) == 0 {
		return "", ""
	}
	if canContinue && rv.rounds < rv.maxRounds {
		return reviewFeedback(blockers, advisories), ""
	}
	if blocking == 0 {
		// Only advisories stand and the repair budget is gone: they were
		// hearsay by construction (unconfirmed, sub-threshold) — pass.
		return "", ""
	}
	rv.blocked = true
	rv.resolvePending(FateExpired)
	return "", fmt.Sprintf("review blockers remain after %d round(s)", rv.rounds)
}

// failOpen records a review-infrastructure failure without blocking: note it,
// and if the gate has produced no findings yet, persist the reason as the
// report's skip field so the telemetry says WHY it contributed nothing.
func (g *gates) failOpen(msg string) {
	g.cfg.Obs.Note(msg)
	if len(g.review.findings) == 0 && g.review.skip == "" {
		g.review.skip = msg
	}
}

// captureDiff writes the CURRENT working tree as a second temp-index tree and
// diffs it against the run-start base — tracked and untracked files included,
// .git and gitignored noise excluded, nothing about the user's index touched.
func (rv *reviewState) captureDiff(ctx context.Context) (string, error) {
	cur, err := vcs.WriteTree(ctx, rv.cfg.Root)
	if err != nil {
		return "", err
	}
	if cur == rv.baseTree {
		return "", nil
	}
	return vcs.DiffTrees(ctx, rv.cfg.Root, rv.baseTree, cur)
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
//     mutates fenced paths is rejected and its damage restored;
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
	data, _, err := readBounded(ctx, g.cfg.Sandbox, fenceRelPath(g.review.alias, f.File))
	if err != nil {
		rf.Fate, rf.DropWhy = FateDropped, fmt.Sprintf("quoted file unreadable: %v", err)
		return rf
	}
	if !strings.Contains(string(data), quote) {
		rf.Fate, rf.DropWhy = FateDropped, "quote does not appear verbatim in "+f.File
		return rf
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
// passed, high confidence), or fence-rejected. False means execution gave no
// signal (the command couldn't start) and the prose ladder continues.
func (g *gates) escalate(ctx context.Context, cmd string, rf *ReviewedFinding) bool {
	out, err := runOp(ctx, g.cfg.verifySandbox(), cmd, g.runTimeout)
	// A repro must never mutate the fence (the reviewer's brief says new files
	// go under /tmp). If it did, undo the damage from the snapshot and reject
	// the finding — the reviewer's sin must not fail the SOLVER's fence gate.
	if g.fence != nil {
		if changed := g.fence.drift(ctx, g.cfg.Sandbox); len(changed) > 0 {
			if rerr := g.fence.restore(ctx, g.cfg.Sandbox, changed); rerr != nil {
				g.cfg.Obs.Note("review: repro mutated fenced paths and restore failed: " + rerr.Error())
			}
			rf.Fate, rf.DropWhy = FateDropped, "repro command modified fenced paths: "+strings.Join(changed, ", ")
			return true
		}
	}
	if err != nil {
		g.cfg.Obs.Note("review: repro could not run (" + err.Error() + ") — falling back to the confidence gate")
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

// NewGate snapshots the workspace's gate baseline. Config needs Sandbox, Root,
// Task, and whichever gate knobs apply (TestFence, VerifyCmd, Reviewer,
// RunTimeout); Model is not used — there is no solver.
func NewGate(ctx context.Context, cfg Config) *Gate {
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}
	// The standalone gate judges a FINISHED tree — there is no "before any changes" to baseline.
	cfg.SkipVerifyBaseline = true
	rt := cfg.RunTimeout
	if rt <= 0 {
		rt = defaultRunTimeout
	}
	return &Gate{g: newGates(ctx, cfg, rt)}
}

// Check runs the composed closing gate once and reports every stage's verdict.
// Stage order and short-circuiting mirror the live loop: the reviewer runs
// ONLY when the fence and VerifyCmd are green (execution-first).
func (t *Gate) Check(ctx context.Context) GateReport {
	rep := GateReport{}
	if rep.FenceViolation = t.g.fenceCheck(ctx); rep.FenceViolation != "" {
		rep.Blocked = true
	} else if rep.VerifyReason = verifyTermination(ctx, t.g.cfg, false, t.g.runTimeout); rep.VerifyReason != "" {
		rep.Blocked = true
	} else if _, blockReason := t.g.reviewFinish(ctx, false); blockReason != "" {
		rep.Blocked = true
	}
	rep.Review = t.g.reviewReport()
	return rep
}
