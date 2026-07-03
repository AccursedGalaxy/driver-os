package agent

// The REVIEW GATE (docs/specs/REVIEW-GATE.md stage 2, slices 1+2): a second closing-gate
// stage after VerifyCmd — a grounded, INDEPENDENT model reviewer whose blocking
// claims are validated deterministically (verbatim-quote re-grounding, a
// confidence hard gate) and, when the reviewer proposes a repro command,
// settled by EXECUTION on the verify sandbox. It exists because execution
// gates saturate before models do (the fasthttp#2272 probe: every model passed
// the full suite under -race; the cheap tier still shipped a goroutine leak
// and an unguarded code path only a reviewer could see).
//
// Import-cycle note: council imports agent, so the MECHANICS live here and the
// reviewer itself is INJECTED via Config.Reviewer — implementations live
// outside agent (council.CodeReviewer, or any custom impl). nil = gate off.

import (
	"context"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

// reviewBlockConfidence is the hard confidence gate: a "blocker" finding below
// it never blocks (Anthropic security-review's ≥8/10; revisit once
// finding-fate calibration data exists). A CONFIRMED repro blocks regardless —
// execution evidence beats self-reported confidence.
const reviewBlockConfidence = 8

// reviewAdviseConfidence is the ADVISORY floor beneath it: an unconfirmed
// blocker in [advise, block) is fed back to the solver for a repair round but
// never blocks the run — the solver hears the finding and decides. Calibration
// case (2026-07-03): a confidence-7 blocker with a correct, concrete failure
// scenario but no runnable repro (a visual TUI defect) died as a silent note
// and the defect shipped; the solver would almost certainly have repaired it
// had it been told. Advisory feedback adds no blocking power, so it cannot
// raise the false-block rate — its only cost is a bounded repair round.
const reviewAdviseConfidence = 5

// DefaultReviewRounds bounds the reviewer↔solver repair loop when
// Config.ReviewRounds is unset. Refine-loop gains concentrate in round 1 with
// documented diminishing returns, and unbounded loops flip correct patches to
// wrong (Self-Refine; Huang ICLR'24) — so the default is deliberately small.
const DefaultReviewRounds = 2

// reviewReproCap bounds how many repro commands the harness executes per
// review round: repro runs are real sandbox executions on the run's clock, and
// a reviewer that proposes ten is nearly always wrong after the second.
const reviewReproCap = 2

// ReviewFinding is one reviewer claim, exactly the structured-verdict schema
// (docs/specs/REVIEW-GATE.md): a verbatim post-patch quote that re-grounds it, a two-value
// severity, a 0-10 confidence, a concrete failure scenario, and an optional
// runnable repro the harness escalates to execution.
type ReviewFinding struct {
	File            string `json:"file"`
	Quote           string `json:"quote"`    // verbatim post-patch code — mismatch drops the finding.
	Severity        string `json:"severity"` // "blocker" | "note"
	Confidence      int    `json:"confidence"`
	FailureScenario string `json:"failure_scenario"`
	ReproCmd        string `json:"repro_cmd,omitempty"` // a command expected to FAIL on this code (slice 2).
}

// ReviewInput is what a Reviewer judges: the task, the solver's unified diff
// vs the run base, the workspace root it may explore READ-ONLY, and the
// deterministic substance signals (fence.go) the harness scanned from the diff.
// It is never told what produced the patch (independence policy).
type ReviewInput struct {
	Task    string
	Diff    string
	Root    string
	Signals []string
	// SessionKey is a run-scoped handle, identical across the rounds of one run
	// and unique per run. A stateful reviewer may key a continuation on it —
	// round 2 re-uses round 1's exploration instead of re-reading the repo
	// (REVIEW-GATE-PLAN §5.3 follow-up (a): the re-read was most of the
	// two-round bill). Empty when the caller doesn't do rounds. Round is the
	// 1-based round number for the same purpose.
	SessionKey string
	Round      int
}

// ReviewVerdict is a Reviewer's structured answer plus its token cost. Model
// is the reviewer's human-facing model id ("" if the impl doesn't know) — it
// flows into ReviewReport.ReviewerModel so finding-fates aggregate PER
// REVIEWER across transcripts (the calibration axis).
type ReviewVerdict struct {
	Findings []ReviewFinding
	Usage    llm.Usage
	Model    string
	// RunID/TranscriptPath identify the reviewer's own persisted sub-run (a
	// reviewer that runs a real agent loop records it like any other run —
	// diagnosing a misbehaving reviewer must not require a temp dump). Empty
	// when the implementation doesn't persist.
	RunID          string
	TranscriptPath string
}

// Reviewer is the injected review-gate implementation (nil = gate off).
// Implementations live OUTSIDE agent (council / cmd wiring) to avoid the
// import cycle; the harness owns validation, escalation, and the repair loop.
type Reviewer interface {
	Review(ctx context.Context, in ReviewInput) (*ReviewVerdict, error)
}

// Finding fates — the calibration telemetry (docs/specs/REVIEW-GATE.md finding 8),
// recorded from day one so reviewer false-positive rates are measurable
// (FP rate = refuted+expired / total blockers).
const (
	FateRepaired = "repaired" // blocked a round, was fed back, and a later round no longer stood in the way.
	FateRefuted  = "refuted"  // its repro command PASSED and confidence was low — the claim was refuted by execution; downgraded to a note.
	FateExpired  = "expired"  // still blocking when the rounds (or the run) ran out.
	FateAdvised  = "advised"  // an unconfirmed blocker under the confidence gate but at/above the advisory floor: fed back for repair, never blocked.
	FateNote     = "note"     // never blocked or fed back: severity "note", or a blocker under the advisory floor.
	FateDropped  = "dropped"  // failed deterministic validation (quote not found verbatim / repro touched the fence).
)

// ReviewedFinding is a finding plus what the harness decided about it.
type ReviewedFinding struct {
	ReviewFinding
	Round     int    `json:"round"`
	Fate      string `json:"fate"`
	Confirmed bool   `json:"confirmed,omitempty"`    // its repro command FAILED — the defect is execution-confirmed.
	ReproOut  string `json:"repro_output,omitempty"` // the (clipped) repro output backing Confirmed/Refuted.
	DropWhy   string `json:"drop_reason,omitempty"`  // why a dropped finding was dropped.
}

// ReviewReport is the run's review-gate record: rounds used, every finding
// with its fate, and the reviewer's token cost. Carried on RunResult.Review,
// the result JSON, and the transcript.
type ReviewReport struct {
	Rounds        int               `json:"rounds"`
	Blocked       bool              `json:"blocked"`                  // the run ended with blockers standing.
	Skipped       string            `json:"skipped,omitempty"`        // why the gate never ran (not a git workspace, reviewer error, …).
	ReviewerModel string            `json:"reviewer_model,omitempty"` // from ReviewVerdict.Model — the per-reviewer calibration axis.
	ReviewerRuns  []string          `json:"reviewer_runs,omitempty"`  // per-round ReviewVerdict.RunID — links this transcript to the reviewer's own.
	Findings      []ReviewedFinding `json:"findings,omitempty"`
	Usage         llm.Usage         `json:"usage"`
}

// ReviewObserver is an OPTIONAL Observer extension, discovered by
// type-assertion like DeltaObserver: a front-end (the TUI) that implements it
// receives typed review-gate events; every other Observer only sees the
// Obs.Note progress lines.
type ReviewObserver interface {
	ReviewStart(round int)
	ReviewFinding(f ReviewFinding)
	ReviewVerdict(blocking int, round int)
}

// reviewState is the run-scoped review gate: the git base tree captured at run
// start, the round budget, and the accumulated findings+fates.
type reviewState struct {
	cfg        Config
	maxRounds  int
	alias      string // model-visible root prefix, for quote-validation path normalization.
	baseTree   string
	skip       string // non-empty => the gate is off for this run, with this recorded reason.
	rounds     int
	blocked    bool
	model      string   // reviewer model id, from the first verdict that names one.
	runIDs     []string // reviewer sub-run IDs, one per verdict that carried one.
	sessionKey string   // run-scoped continuation handle handed to every round (ReviewInput.SessionKey).
	findings   []ReviewedFinding
	pending    []int // indices into findings: blockers fed back, awaiting repaired/expired resolution.
	usage      llm.Usage
}

// newReviewState captures the diff BASELINE at run start: a git tree object of
// the whole working tree (tracked + untracked, gitignore-respected) written
// through a TEMPORARY index — the workspace's real index and HEAD are never
// touched, and a dirty tree is fine (the diff is vs the recorded start state,
// not vs HEAD). A non-git workspace records a skip reason and never blocks.
func newReviewState(ctx context.Context, cfg Config) *reviewState {
	if cfg.Reviewer == nil {
		return nil
	}
	// sessionKey is a fresh nonce per run (not the run ID — the state exists
	// before the result is stamped); its only contract is same-run stability.
	rv := &reviewState{cfg: cfg, maxRounds: cfg.ReviewRounds, alias: sandboxAlias(cfg.Sandbox), sessionKey: newRunID()}
	if rv.maxRounds <= 0 {
		rv.maxRounds = DefaultReviewRounds
	}
	switch {
	case cfg.Root == "":
		rv.skip = "review skipped: no workspace root configured"
	case !vcs.IsRepo(ctx, cfg.Root):
		rv.skip = "review skipped: not a git workspace"
	default:
		tree, err := vcs.WriteTree(ctx, cfg.Root)
		if err != nil {
			rv.skip = "review skipped: could not capture the base tree: " + err.Error()
		} else {
			rv.baseTree = tree
		}
	}
	if rv.skip != "" && cfg.Obs != nil {
		cfg.Obs.Note(rv.skip)
	}
	return rv
}

// report renders the terminal ReviewReport (nil when the gate was off). Any
// still-pending blockers expire here — the "fed back but the run died before
// re-review" path.
func (rv *reviewState) report() *ReviewReport {
	if rv == nil {
		return nil
	}
	rv.resolvePending(FateExpired)
	return &ReviewReport{
		Rounds:        rv.rounds,
		Blocked:       rv.blocked,
		Skipped:       rv.skip,
		ReviewerModel: rv.model,
		ReviewerRuns:  rv.runIDs,
		Findings:      rv.findings,
		Usage:         rv.usage,
	}
}

// resolvePending assigns fate to the previous round's fed-back blockers.
func (rv *reviewState) resolvePending(fate string) {
	for _, i := range rv.pending {
		if rv.findings[i].Fate == "" {
			rv.findings[i].Fate = fate
		}
	}
	rv.pending = nil
}

// reviewObserver extracts the optional typed-event sink from the observer.
func reviewObserver(obs Observer) ReviewObserver {
	if ro, ok := obs.(ReviewObserver); ok {
		return ro
	}
	return nil
}
