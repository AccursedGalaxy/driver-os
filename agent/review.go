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
	"errors"

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

// ReviewStatus is the terminal review-gate classification recorded in
// ReviewReport.Status.
type ReviewStatus string

const (
	ReviewClean       ReviewStatus = "clean"
	ReviewBlocked     ReviewStatus = "blocked"
	ReviewAdvisory    ReviewStatus = "advisory"
	ReviewUnavailable ReviewStatus = "unavailable"
	ReviewParseError  ReviewStatus = "parse_error"
	ReviewTimeout     ReviewStatus = "timeout"
	ReviewCanceled    ReviewStatus = "canceled"
)

// ErrReviewParse marks reviewer output that could not be parsed even after the
// reviewer implementation's corrective retry.
var ErrReviewParse = errors.New("review parse error")

// ReviewFinding is one reviewer claim, exactly the structured-verdict schema
// (docs/specs/REVIEW-GATE.md): a verbatim post-patch quote that re-grounds it, a two-value
// severity, a 0-10 confidence, a concrete failure scenario, and an optional
// runnable repro the harness escalates to execution.
type ReviewFinding struct {
	ID              string `json:"id,omitempty"` // stable within a run; evidence summaries use it as a foreign key.
	File            string `json:"file"`
	Quote           string `json:"quote"`    // verbatim post-patch code — mismatch drops the finding.
	Severity        string `json:"severity"` // "blocker" | "note"
	Confidence      int    `json:"confidence"`
	FailureScenario string `json:"failure_scenario"`
	ReproCmd        string `json:"repro_cmd,omitempty"` // a command expected to FAIL on this code (slice 2).

	// NoReproReason is set when the reviewer was asked for a repro command
	// (via ReproSolicitor) but explicitly declined with a reason — the defect
	// cannot be demonstrated by a runnable command (e.g. a visual TUI defect).
	// A finding with a NoReproReason expires as today (unconfirmed).
	NoReproReason string `json:"no_repro_reason,omitempty"`

	// Severity-rubric fields (slice 2b) — the reviewer is asked to fill these
	// to make the gate's JSON report self-describing without re-reading the
	// full finding prose. Missing/invalid values parse as empty and never
	// affect the blocking decision.
	Category     string `json:"category,omitempty"`     // correctness|performance|security|maintainability|style
	Impact       string `json:"impact,omitempty"`       // high|medium|low
	Reproducible string `json:"reproducible,omitempty"` // "true"|"false" — could a runnable command demonstrate it?
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
	Summary  string // reviewer free-prose preamble before the findings array, when present.
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

// ReproSolicitor is an optional Reviewer extension: after a review round
// produces blocker findings without repro commands, the harness can ask the
// reviewer to supply runnable repro commands in a single follow-up call per
// round. The reviewer receives the batched list of eligible findings and
// returns each with either a repro_cmd or an explicit no_repro_reason.
// Implementations that don't implement this are simply not consulted —
// findings without repro commands expire as today. The harness discovers it
// via type assertion; the Reviewer interface itself stays unchanged so
// existing implementations (including the fake in tests) compile untouched.
type ReproSolicitor interface {
	SolicitRepro(ctx context.Context, in ReviewInput, findings []ReviewFinding) ([]ReviewFinding, llm.Usage, error)
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
	FateDropped  = "dropped"  // failed deterministic validation (quote not found verbatim / repro dirtied the workspace).
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
	Status        ReviewStatus      `json:"status,omitempty"`
	Blocked       bool              `json:"blocked"`                  // the run ended with blockers standing.
	Salvage       bool              `json:"salvage,omitempty"`        // advisory review on an Unverified run; never changed outcome.
	Skipped       string            `json:"skipped,omitempty"`        // why the gate never ran (not a git workspace, reviewer error, …).
	ReviewerModel string            `json:"reviewer_model,omitempty"` // from ReviewVerdict.Model — the per-reviewer calibration axis.
	ReviewerRuns  []string          `json:"reviewer_runs,omitempty"`  // per-round ReviewVerdict.RunID — links this transcript to the reviewer's own.
	Summaries     []string          `json:"summaries,omitempty"`      // per-round free-prose preambles; "" when absent.
	Findings      []ReviewedFinding `json:"findings,omitempty"`
	Usage         llm.Usage         `json:"usage"`

	// ConfirmedBlockers counts blocker findings whose repro command FAILED
	// (Confirmed=true, execution-evidenced). UnconfirmedBlockers counts
	// blocker findings that stood without execution evidence — either the
	// reviewer supplied no repro command, the command passed (and confidence
	// was high enough not to refute), or the repro couldn't run. A caller
	// can tell CAUGHT-with-confirmed-repro from CAUGHT-on-unconfirmed-claims
	// at a glance.
	ConfirmedBlockers   int `json:"confirmed_blockers"`
	UnconfirmedBlockers int `json:"unconfirmed_blockers"`
}

// ReviewObserver is an OPTIONAL Observer extension, discovered by
// type-assertion like DeltaObserver: a front-end (the TUI) that implements it
// receives typed review-gate events; every other Observer only sees the
// Obs.Note progress lines.
type ReviewObserver interface {
	ReviewStart(round int)
	ReviewFinding(f ReviewFinding)
	ReviewVerdict(blocking int, round int, summary string)
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
	status     ReviewStatus
	blocked    bool
	salvage    bool
	model      string   // reviewer model id, from the first verdict that names one.
	summaries  []string // one entry per round, blank when the reviewer emitted bare JSON.
	runIDs     []string // reviewer sub-run IDs, one per verdict that carried one.
	sessionKey string   // run-scoped continuation handle handed to every round (ReviewInput.SessionKey).
	findings   []ReviewedFinding
	pending    []int // indices into findings: blockers fed back, awaiting repaired/expired resolution.
	usage      llm.Usage
}

// SetupError is a typed pre-solver configuration failure. Callers should report
// it as a setup error rather than as a model/provider run result.
type SetupError struct {
	Kind string
	Msg  string
}

func (e *SetupError) Error() string { return e.Msg }

func setupErr(kind, msg string) error { return &SetupError{Kind: kind, Msg: msg} }

// newReviewState captures the diff BASELINE at run start: a git tree object of
// the whole working tree (tracked + untracked, gitignore-respected) written
// through a TEMPORARY index — the workspace's real index and HEAD are never
// touched, and a dirty tree is fine (the diff is vs the recorded start state,
// not vs HEAD). A non-git workspace records a skip reason only when review is
// optional; otherwise an armed-but-inoperable gate is a setup error.
func newReviewState(ctx context.Context, cfg Config) (*reviewState, error) {
	if cfg.Reviewer == nil {
		return nil, nil
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
	if rv.skip != "" {
		if !cfg.ReviewPolicy.optional() {
			return nil, setupErr("review_unavailable", rv.skip+" (review gate was armed; pass -review-optional to skip and continue)")
		}
		rv.status = ReviewUnavailable
		if cfg.Obs != nil {
			cfg.Obs.Note(rv.skip)
		}
	}
	return rv, nil
}

// report renders the terminal ReviewReport (nil when the gate was off). Any
// still-pending blockers expire here — the "fed back but the run died before
// re-review" path.
func (rv *reviewState) report() *ReviewReport {
	if rv == nil {
		return nil
	}
	rv.resolvePending(FateExpired)
	var confirmed, unconfirmed int
	for _, f := range rv.findings {
		if f.Severity != "blocker" {
			continue
		}
		if f.Confirmed {
			confirmed++
		} else {
			unconfirmed++
		}
	}
	return &ReviewReport{
		Rounds:              rv.rounds,
		Status:              rv.status,
		Blocked:             rv.blocked,
		Salvage:             rv.salvage,
		Skipped:             rv.skip,
		ReviewerModel:       rv.model,
		ReviewerRuns:        rv.runIDs,
		Summaries:           rv.summaries,
		Findings:            rv.findings,
		Usage:               rv.usage,
		ConfirmedBlockers:   confirmed,
		UnconfirmedBlockers: unconfirmed,
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

// findRecurringConfirmedBlocker checks the current round's pending (blocking)
// findings for a CONFIRMED blocker whose File+Quote exactly match a confirmed
// finding from an earlier round — the solver saw it, tried, and couldn't fix
// it. Returns (current index, earlier index, ok). When ok, the caller should
// stop the repair loop: another round won't help.
func (rv *reviewState) findRecurringConfirmedBlocker() (int, int, bool) {
	for _, idx := range rv.pending {
		rf := &rv.findings[idx]
		if !rf.Confirmed {
			continue
		}
		for j := range rv.findings {
			prev := &rv.findings[j]
			if prev.Round >= rv.rounds || !prev.Confirmed {
				continue
			}
			if prev.File != rf.File || prev.Quote != rf.Quote {
				continue
			}
			return idx, j, true
		}
	}
	return 0, 0, false
}

// reviewObserver extracts the optional typed-event sink from the observer.
func reviewObserver(obs Observer) ReviewObserver {
	if ro, ok := obs.(ReviewObserver); ok {
		return ro
	}
	return nil
}
