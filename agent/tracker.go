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
	"fmt"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/internal/runspec"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// VerificationCause refines EvidenceStatus without creating a second status
// taxonomy. It records why a check was failed or inconclusive.
type VerificationCause string

const (
	VerificationCauseNone           VerificationCause = ""
	VerificationCauseTestFailure    VerificationCause = "test-failure"
	VerificationCauseTimeout        VerificationCause = "timeout"
	VerificationCauseEnvironment    VerificationCause = "environment-fault"
	VerificationCauseCancellation   VerificationCause = "cancellation"
	VerificationCauseExecutionError VerificationCause = "execution-error"
)

// VerificationRecord is the single standing-context record for either a
// model-issued check or the configured gate. Tree identifies the exact workspace
// snapshot measured by the command — it is what the staleness label keys on —
// and Attempts counts gate recurrences so replacement ordering is explicit.
type VerificationRecord struct {
	Status      EvidenceStatus    `json:"status"`
	Cause       VerificationCause `json:"cause,omitempty"`
	Command     string            `json:"command"`
	Output      string            `json:"output"`
	Fingerprint string            `json:"fingerprint"`
	Tree        string            `json:"tree"`
	Attempts    int               `json:"attempts"`
}

func classifyRunObservation(obs string) (EvidenceStatus, VerificationCause) {
	switch {
	case isRunSuccess(obs):
		return EvidencePassed, VerificationCauseNone
	case isRunTimeout(obs):
		return EvidenceInconclusive, VerificationCauseTimeout
	case isInfraFault(obs):
		return EvidenceInconclusive, VerificationCauseEnvironment
	case strings.HasPrefix(obs, "ERROR:") && strings.Contains(strings.ToLower(obs), "cancel"):
		return EvidenceInconclusive, VerificationCauseCancellation
	case strings.HasPrefix(obs, "ERROR:"):
		return EvidenceInconclusive, VerificationCauseExecutionError
	case isRunFailure(obs):
		return EvidenceFailed, VerificationCauseTestFailure
	default:
		return EvidenceInconclusive, VerificationCauseExecutionError
	}
}

func newVerificationRecord(cmd, obs, tree string) VerificationRecord {
	status, cause := classifyRunObservation(obs)
	return VerificationRecord{
		Status: status, Cause: cause, Command: strings.TrimSpace(cmd),
		Output: tailClip(obs, standingGateTailCap), Fingerprint: runFingerprint(obs),
		Tree: tree, Attempts: 1,
	}
}

// trackerDeps is the tracker's by-value slice of the resolved policy plus the
// two runtime handles its detectors need (PROFILES.md §7.1: components receive
// small per-domain projections, not the whole spec).
type trackerDeps struct {
	verifyCmd          string // the ARMED gate command (post auto-verify derivation) — recordRun keys the authoritative gate record on it.
	churnNudgeRuns     int
	diagnoseCmd        string
	diagnoseAfterEdits int
	finishNudgeWindow  int
	verifySB           sandbox.Sandbox
	obs                Observer
}

type turnTracker struct {
	deps     trackerDeps
	maxIter  int
	policy   TerminationPolicy
	counters *DetectorCounters

	// (P5) Stagnant-observation detector state + the last-run flags the
	// verification fallback (VerifyLastRun) and HP-4 finisher read. lastRunFP
	// fingerprints the most recent failing `run` result (duration and volatile
	// numbers stripped — see runFingerprint); stagnant counts identical recurrences.
	lastRunFP     string
	stagnant      int
	lastRunFailed bool
	lastRunPassed bool

	// Standing context records: generic last run and authoritative gate run are
	// independent so a later non-gate command cannot clobber the last verify status.
	lastRun    VerificationRecord
	lastVerify VerificationRecord

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

	// Observation-repeat detector: tracks a fully observed action fingerprint
	// (tool call signature + the observation text the model would read) across
	// consecutive turns. This is the hard backstop for reasoning providers whose
	// opaque trace moves while the outside world does not.
	lastToolObsFP string
	toolObsRepeat int

	// Green-repeat detector: tracks the runFingerprint of the last PASSING run
	// and how many times the SAME green result recurred with no file mutation
	// between. A model that keeps re-running the same passing command without
	// changing anything is spinning — it needs a nudge, not a kill.
	lastGreenFP string
	greenRepeat int
	greenNudged bool
}

func newTurnTracker(deps trackerDeps, maxIter int, policy TerminationPolicy, counters *DetectorCounters) *turnTracker {
	return &turnTracker{deps: deps, maxIter: maxIter, policy: policy, counters: counters}
}

// newTrackerDeps snapshots the tracker's policy slice. Called AFTER auto-verify
// resolution so vs.Cmd is the armed gate command, exactly like the old
// post-resolveAutoVerify Config copy.
func newTrackerDeps(pol runspec.PolicyValue, vs *verifyState, rt Runtime) trackerDeps {
	return trackerDeps{
		verifyCmd:          vs.Cmd,
		churnNudgeRuns:     pol.ChurnNudgeRuns,
		diagnoseCmd:        pol.DiagnoseCmd,
		diagnoseAfterEdits: pol.DiagnoseAfterEdits,
		finishNudgeWindow:  pol.FinishNudgeWindow,
		verifySB:           rt.verifySandbox(),
		obs:                rt.Obs,
	}
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
	if t.lastRunFailed && isInfraFault(obs) {
		// An environment fault is NO SIGNAL, not progress: don't count it as
		// a failure (no spiral/churn credit), but don't reset the stagnation
		// streak either — an intermittent quota blip must not launder a
		// genuine stall back to zero.
		t.lastRunFailed = false
		return false, t.stagnant
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

	return t.stagnant >= t.policy.MaxStagnant, t.stagnant
}

func (t *turnTracker) recordRun(cmd, obs, tree string) {
	record := newVerificationRecord(cmd, obs, tree)
	t.lastRun = record
	verify := strings.TrimSpace(t.deps.verifyCmd)
	if verify != "" && record.Command == verify {
		record.Attempts = t.lastVerify.Attempts + 1
		t.lastVerify = record
	}
}

// observeToolObservation ingests a complete action+observation fingerprint for a
// committed tool turn. Reasoning-only movement is deliberately NOT part of this
// key: if the same call produces the same observation maxReasoningRepeats times in
// a row, the outside world is not changing, so kill even for a thinking model.
// The count is occurrences (not repeats-after-first), matching the backlog's
// "N consecutive times" wording and giving a max-iteration run set to the hard
// ceiling a chance to terminate as killed_repeat instead of hit_cap.
func (t *turnTracker) observeToolObservation(fp string) (kill bool, count int) {
	if fp == "" {
		t.lastToolObsFP, t.toolObsRepeat = "", 0
		return false, 0
	}
	if fp == t.lastToolObsFP {
		t.toolObsRepeat++
	} else {
		t.lastToolObsFP, t.toolObsRepeat = fp, 1
	}
	return t.toolObsRepeat >= t.policy.MaxReasoningRepeats, t.toolObsRepeat
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
	if t.nudged || t.deps.churnNudgeRuns <= 0 {
		return false
	}
	if t.failRuns >= t.deps.churnNudgeRuns || t.edits >= t.deps.churnNudgeRuns {
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
	if t.deps.diagnoseCmd == "" || t.deps.diagnoseAfterEdits <= 0 || t.editsSinceGreen < t.deps.diagnoseAfterEdits {
		return ""
	}
	switch report, state := diagnoseSource(ctx, t.deps.diagnoseCmd, t.deps.verifySB, runTimeout); state {
	case diagClean:
		t.editsSinceGreen = 0
	case diagDirty:
		t.deps.obs.Note("stuck with a broken build — surfacing diagnostics (code-intel slice 1)")
		return diagnosticsMessage(t.deps.diagnoseCmd, report)
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
	w := t.deps.finishNudgeWindow
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
	if t.greenRepeat >= t.policy.GreenRepeatThreshold {
		t.greenNudged = true
		return true
	}
	return false
}

// repeatThreshold is the reasoning-aware tight-loop ceiling shared by both
// loops: a turn whose reasoning trace ADVANCED gets the lenient threshold (see
// maxReasoningRepeats for the rationale and the known Gemini tradeoff); a
// frozen or absent trace keeps the strict one.
func repeatThreshold(policy TerminationPolicy, reasoningAdvanced bool) int {
	if reasoningAdvanced {
		return policy.MaxReasoningRepeats
	}
	return policy.MaxRepeats
}

// spiralFrontierCap bounds the visited-target set the explore-spiral detector
// keeps per run (deliverable 1b): a rolling FIFO window of normalized discovery
// targets. It only has to be large enough that a genuine top-down orientation of
// a big repo never wraps mid-sweep; 64 distinct list_dir paths + search queries
// is far more than any real orientation burst and keeps the set cheap.
const spiralFrontierCap = 64

// spiralWanderMultiple is the hard-wandering bound expressed as a multiple of the
// cycle window (deliverable 1c): even discovery turns that keep revealing NEW
// frontier targets — never revisiting, so the cycle counter never trips — must
// eventually die if the model never follows a pointer. At the default window of
// 4 this caps endless novel wandering at 16 discovery-only turns. Deterministic:
// same for every model, reasoning behavior, and benchmark case (deliverable 4).
const spiralWanderMultiple = 4

// spiralState is the frontier/state-aware explore-spiral detector shared by both
// loops (native per-turn, text per-call) so they cannot drift (deliverable 3).
// It replaces the old "N consecutive discovery-only turns = no progress" rule
// (which false-killed textbook top-down orientation of a large repo — the
// opa-8781 P1) with a deterministic state machine:
//
//   - A discovery turn that reveals ANY new frontier target (a list_dir path or
//     search query not seen before) is orientation, not spinning: it resets the
//     cycle counter (deliverable 1a/1b).
//   - A discovery turn whose targets were ALL visited before is a cycle: it
//     counts toward the cycle kill at the window (deliverable 1b).
//   - A hard wandering bound (spiralWanderMultiple × window) caps even endless
//     novel-target discovery so the run always terminates (deliverable 1c).
//
// Any non-discovery turn (a read_file, run/diagnostics, or workspace mutation)
// is a phase transition that resets the whole state via reset() — the first
// useful read starts a fresh orientation budget (deliverable 1a/1e). Both bounds
// are deterministic and identical for every model and case; the reasoning-aware
// 2× leniency stays with the tight-loop repeat detector (repeatThreshold), which
// is the only place it still applies (deliverable 2/4).
type spiralState struct {
	policy   TerminationPolicy
	counters *DetectorCounters
	window   int             // cycle window (Config.NavSpiralWindow, default noProgressWindow).
	visited  map[string]bool // bounded frontier of discovery targets seen this run.
	order    []string        // insertion order, for FIFO eviction at spiralFrontierCap.
	cycleRun int             // consecutive all-visited (revisiting) discovery turns.
	wander   int             // consecutive discovery-only turns (novel or not).
}

type spiralKillKind uint8

const (
	spiralKillNone spiralKillKind = iota
	spiralKillCycle
	spiralKillWander
)

func newSpiralState(policy TerminationPolicy, counters *DetectorCounters) *spiralState {
	return &spiralState{policy: policy, counters: counters, window: policy.NavSpiralWindow, visited: make(map[string]bool)}
}

// observeDiscoveryTurn ingests the normalized target set of a discovery-ONLY turn
// (list_dir paths + search queries; a parallel fan-out is one turn, several
// targets) and reports whether the run should end as KilledSpiral, with the
// reason. A turn that adds any new frontier entry resets the cycle counter; a
// turn revisiting only seen targets advances it. The wander counter advances on
// every discovery turn regardless.
func (s *spiralState) observeDiscoveryTurn(targets []string) (kind spiralKillKind, reason string) {
	novel := false
	for _, tgt := range targets {
		if !s.visited[tgt] {
			novel = true
			s.add(tgt)
		}
	}
	s.wander++
	if novel {
		s.cycleRun = 0
	} else {
		s.cycleRun++
	}
	if s.cycleRun >= s.window {
		if s.counters != nil {
			s.counters.SpiralCycle++
		}
		return spiralKillCycle, fmt.Sprintf("no progress: %d discovery turns revisiting only already-seen targets (list_dir/search) — read or edit a specific target, or answer", s.cycleRun)
	}
	if s.wander >= s.policy.WanderMultiple*s.window {
		if s.counters != nil {
			s.counters.SpiralWander++
		}
		return spiralKillWander, fmt.Sprintf("no progress: %d discovery-only turns without reading, editing, or running anything — follow a pointer to a concrete target, or answer", s.wander)
	}
	return spiralKillNone, ""
}

// add records a new frontier target, evicting the oldest at the cap so the set
// stays bounded (deliverable 1b).
func (s *spiralState) add(tgt string) {
	s.visited[tgt] = true
	s.order = append(s.order, tgt)
	if len(s.order) > s.policy.FrontierCap {
		delete(s.visited, s.order[0])
		s.order = s.order[1:]
	}
}

// reset clears the discovery state on any progress / phase-transition turn (a
// read_file, run/diagnostics, or workspace mutation): the first useful read of a
// long orientation burst restarts the frontier and both counters, so the model
// gets a fresh orientation budget after it commits to a concrete target
// (deliverable 1a/1e).
func (s *spiralState) reset() {
	s.cycleRun, s.wander = 0, 0
	s.visited = make(map[string]bool)
	s.order = nil
}
