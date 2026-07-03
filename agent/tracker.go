package agent

// turnTracker owns the detector/nudge state that Run and RunNative previously
// each maintained by hand (review #9): the stagnant-observation detector, the
// churn signals, the diagnostics-feed stuck counter, and HP-4's near-cap
// finisher. The two copies had already drifted once (the malformed-transcript
// kill, harness review finding #1, existed only in the native loop) — one
// struct makes the next divergence a compile error instead of a dogfood
// finding. Only the WIRE-FORMAT differences stay in the loops: how an
// observation is fed back (appended text vs a standalone user message), and
// per-call (text) vs per-turn (native) evaluation of the action-keyed
// detectors, whose state (lastAction/lastTurnSig, repeats, spiral runs) is
// deliberately loop-local.

import (
	"context"
	"time"
)

type turnTracker struct {
	cfg     Config
	maxIter int

	// (P5) Stagnant-observation detector state + the last-run flags the
	// verification fallback (VerifyLastRun) and HP-4 finisher read. lastRunFP
	// fingerprints the most recent failing `run` result (duration and volatile
	// numbers stripped — see runFingerprint); stagnant counts identical recurrences.
	lastRunFP     string
	stagnant      int
	lastRunFailed bool
	lastRunPassed bool

	// Churn signals + the once-only latch (see Config.ChurnNudgeRuns).
	failRuns int // failing `run` results this session.
	edits    int // edit_file calls this session.
	nudged   bool

	// (code-intel slice 1) edits since the last green build/run — the
	// diagnostics-feed stuck signal (reset on a passing `run` or a clean check).
	editsSinceGreen int

	// (HP-4) Near-cap finisher: lastEditIter is the iteration of the most
	// recent file mutation (0 = none), so i-lastEditIter measures how long the
	// files have been stable; finishNudged fires the hint at most once.
	lastEditIter int
	finishNudged bool

	// Green-repeat detector: tracks the runFingerprint of the last PASSING run
	// and how many times the SAME green result recurred with no file mutation
	// between. A model that keeps re-running the same passing command without
	// changing anything is spinning — it needs a nudge, not a kill.
	lastGreenFP  string
	greenRepeat  int
	greenNudged  bool
}

func newTurnTracker(cfg Config, maxIter int) *turnTracker {
	return &turnTracker{cfg: cfg, maxIter: maxIter}
}

// observeRun ingests one `run` observation. A `run` that keeps FAILING with the
// byte-identical result, despite the model changing actions between, is a stall
// the action-keyed detectors can't see — so it is tracked on the OBSERVATION,
// not the action. Returns whether the stagnant detector should kill the run and
// the recurrence count (for the kill message).
func (t *turnTracker) observeRun(obs string) (kill bool, count int) {
	t.lastRunFailed = isRunFailure(obs)
	t.lastRunPassed = isRunSuccess(obs) // (HP-4) the green/red of the most recent run.
	if t.lastRunPassed {
		t.editsSinceGreen = 0 // (code-intel slice 1) reaching green clears the stuck count.
	}
	if t.lastRunFailed {
		t.failRuns++
	}
	switch {
	case !t.lastRunFailed: // a passing run is real progress — reset.
		t.stagnant, t.lastRunFP = 0, ""
	case runFingerprint(obs) == t.lastRunFP:
		t.stagnant++
	default: // a NEW failure — the world changed, count restarts.
		t.stagnant, t.lastRunFP = 1, runFingerprint(obs)
	}

	// Green-repeat detector: track consecutive IDENTICAL passing runs with no
	// file mutation between them. A DIFFERENT green fingerprint resets to 1
	// (it's a new command, not a re-run of the same thing); a failing run
	// resets entirely.
	if t.lastRunPassed {
		fp := runFingerprint(obs)
		if fp == t.lastGreenFP {
			t.greenRepeat++
		} else {
			t.greenRepeat = 1
			t.lastGreenFP = fp
		}
	} else {
		t.greenRepeat, t.lastGreenFP = 0, ""
	}

	return t.stagnant >= maxStagnant, t.stagnant
}

// observeAction records a dispatched action's mutation/churn signals at
// iteration i and reports whether it mutated a file (the native loop's
// editedThisTurn signal).
func (t *turnTracker) observeAction(i int, verb string) (mutated bool) {
	if verb == "edit_file" {
		t.edits++
	}
	if verb == "write_file" || verb == "edit_file" {
		t.lastEditIter = i // (HP-4) a file mutation resets the "files stable" clock.
		t.editsSinceGreen++
		t.greenRepeat = 0 // the world changed — a re-run of the same green command is now legitimate.
		return true
	}
	return false
}

// churnNudge reports whether the ONE-TIME churn hint fires now (P3, latching):
// either wandering signal — repeated failing test-runs (the gpt-oss churn) or
// many edit_file calls without converging (the grok read/edit churn, which
// barely runs the tests so a run-only trigger never fires) — crossed the
// threshold. The caller appends churnNudge to whatever the model reads next.
func (t *turnTracker) churnNudge() bool {
	if t.nudged || t.cfg.ChurnNudgeRuns <= 0 {
		return false
	}
	if t.failRuns >= t.cfg.ChurnNudgeRuns || t.edits >= t.cfg.ChurnNudgeRuns {
		t.nudged = true
		return true
	}
	return false
}

// diagnostics runs the (code-intel slice 1) feed when it is DUE — the model has
// edited DiagnoseAfterEdits times without reaching a green build — and returns
// the rendered message to surface, or "" (not due / clean / unknown). A clean
// check resets the stuck counter and stays silent; an infra fault (diagUnknown)
// keeps the counter and stays silent — it is not a green build. The caller
// decides the wire format (appended to the observation vs a standalone user
// message).
func (t *turnTracker) diagnostics(ctx context.Context, runTimeout time.Duration) string {
	if t.cfg.DiagnoseCmd == "" || t.cfg.DiagnoseAfterEdits <= 0 || t.editsSinceGreen < t.cfg.DiagnoseAfterEdits {
		return ""
	}
	switch report, state := diagnoseSource(ctx, t.cfg, runTimeout); state {
	case diagClean:
		t.editsSinceGreen = 0
	case diagDirty:
		t.cfg.Obs.Note("stuck with a broken build — surfacing diagnostics (code-intel slice 1)")
		return diagnosticsMessage(t.cfg.DiagnoseCmd, report)
	}
	return ""
}

// finishNudge reports whether HP-4's near-cap finisher fires now (latching):
// the budget is nearly spent AND the world looks SETTLED — the most recent
// `run` was green and no file has been mutated for the window — so inject the
// one-time hint that manufactures the finish ATTEMPT a spinner never makes.
// The i < maxIter guard guarantees at least one more turn to act on it (a hint
// on the very last turn would be wasted). See Config.FinishNudgeWindow.
func (t *turnTracker) finishNudge(i int) bool {
	w := t.cfg.FinishNudgeWindow
	if t.finishNudged || w <= 0 || i >= t.maxIter {
		return false
	}
	if t.maxIter-i <= w && t.lastRunPassed && i-t.lastEditIter >= w {
		t.finishNudged = true
		return true
	}
	return false
}

// greenRepeatNudge reports whether the ONE-TIME green-repeat hint fires now
// (latching): the model has re-run the SAME passing command 3+ times with NO
// file mutation between runs. It is a nudge only — never a kill — because a
// thinking model re-running a green test is the benign side of the coin whose
// malicious side is the stagnant-observation kill (the two-threshold detector
// comments in agent.go explain the measured false-kill tradeoff). The caller
// appends greenRepeatNudgeText to whatever the model reads next.
func (t *turnTracker) greenRepeatNudge() bool {
	if t.greenNudged {
		return false
	}
	if t.greenRepeat >= 3 {
		t.greenNudged = true
		return true
	}
	return false
}

// repeatThreshold is the reasoning-aware tight-loop ceiling shared by both
// loops: a turn whose reasoning trace ADVANCED gets the lenient threshold (see
// maxReasoningRepeats for the rationale and the known Gemini tradeoff); a
// frozen or absent trace keeps the strict one.
func repeatThreshold(reasoningAdvanced bool) int {
	if reasoningAdvanced {
		return maxReasoningRepeats
	}
	return maxRepeats
}

// spiralLimit is the reasoning-aware discovery-spiral window shared by both
// loops: a discovery turn whose reasoning trace moved gets 2× the window (a
// model visibly working through fresh listings is orienting, not spinning —
// the measured glm-5 false-kill); a frozen or absent trace keeps the strict one.
func spiralLimit(window int, reasoningAdvanced bool) int {
	if reasoningAdvanced {
		return 2 * window
	}
	return window
}
