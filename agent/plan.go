package agent

// The PLAN stage (triad slice 3; docs/findings/EFFORT-REBASELINE.md): an
// OPENING stage mirroring the closing review gate. A separate, independently
// configured planner model explores the workspace READ-ONLY and emits an
// implementation plan; the plan is appended to the solver's task before the
// first solver turn. It exists because effort pays at architecture-CHOICE
// points, not the edit grind (the re-baseline data): the plan is where the
// approach is decided, so that is where the strong model belongs — the solver
// then executes it at its own (cheaper) tier.
//
// Import-cycle note: exactly the Reviewer pattern — council imports agent, so
// the MECHANICS live here and the planner itself is INJECTED via
// Config.Planner (council.Planner, or any custom impl). nil = stage off.
//
// The stage FAILS OPEN: a planner error (or an empty plan) never blocks the
// run — the solver proceeds unplanned, exactly as if the stage were off, with
// the reason recorded on PlanReport.Skipped. A plan is an aid, not a gate.

import (
	"context"
	"strings"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// PlanInput is what a Planner plans: the task and the workspace root it may
// explore READ-ONLY. Continuing marks a follow-up turn of an ongoing
// conversation (Config.History non-empty) — the task may reference earlier
// exchanges the planner cannot see, so it should plan from what the code
// itself can ground.
type PlanInput struct {
	Task       string
	Root       string
	Continuing bool
}

// PlanResult is a Planner's answer plus its token cost. Model is the planner's
// human-facing model id ("" if the impl doesn't know); RunID/TranscriptPath
// identify the planner's own persisted sub-run (empty when the implementation
// doesn't persist) — the same telemetry contract as ReviewVerdict.
type PlanResult struct {
	Plan           string
	Model          string
	Usage          llm.Usage
	RunID          string
	TranscriptPath string
}

// Planner is the injected plan-stage implementation (nil = stage off).
// Implementations live OUTSIDE agent (council.Planner, or cmd wiring) to avoid
// the import cycle; the harness owns the injection point and the fail-open
// policy.
type Planner interface {
	Plan(ctx context.Context, in PlanInput) (*PlanResult, error)
}

// PlanReport is the run's plan-stage record: the plan the solver was handed
// (or why there was none) and the planner's own token cost. Carried on
// RunResult.Plan and the transcript. Usage is deliberately SEPARATE from
// RunResult.Usage (the solver's bill), mirroring ReviewReport.Usage — per-role
// cost must stay attributable.
type PlanReport struct {
	Model      string    `json:"model,omitempty"`
	Plan       string    `json:"plan,omitempty"`
	Skipped    string    `json:"skipped,omitempty"` // why the solver ran unplanned (planner error / empty plan).
	PlannerRun string    `json:"planner_run,omitempty"`
	Usage      llm.Usage `json:"usage"`
}

// PlanObserver is an OPTIONAL Observer extension, discovered by type-assertion
// like ReviewObserver: a front-end that implements it receives the typed
// plan-stage events (the TUI renders the plan as its own block); every other
// Observer only sees the Obs.Note progress lines.
type PlanObserver interface {
	PlanStart()
	PlanDone(plan, skipped string)
}

// planHeader frames the plan inside the solver's task. "Follow unless the code
// contradicts it" on purpose: the planner read the same tree but decides from
// less context than the solver accumulates — a plan is authority on APPROACH,
// never on facts the solver can re-read.
const planHeader = "\n\n----\nIMPLEMENTATION PLAN — prepared by a separate planning pass that explored this repository read-only. Follow its approach unless the code you read contradicts it; when you deviate, say why:\n\n"

// runPlanStage runs the opening plan stage and returns the task the solver
// should be seeded with (the original task, plan-augmented on success) plus
// the stage's report. nil Planner = stage off: the task passes through
// untouched and the report is nil, keeping the no-planner path byte-identical.
// Called by both loops after Obs is defaulted and before seedMessages.
func runPlanStage(ctx context.Context, cfg Config) (string, *PlanReport) {
	if cfg.Planner == nil {
		return cfg.Task, nil
	}
	rep := &PlanReport{}
	po, _ := cfg.Obs.(PlanObserver)
	if po != nil {
		po.PlanStart()
	}
	cfg.Obs.Note("plan: preparing an implementation plan…")
	pctx, pcancel := gateContext(ctx, planTimeout)
	defer pcancel()
	pr, err := cfg.Planner.Plan(pctx, PlanInput{Task: cfg.Task, Root: cfg.Root, Continuing: len(cfg.History) > 0})
	var plan string
	if pr != nil {
		rep.Model, rep.Usage, rep.PlannerRun = pr.Model, pr.Usage, pr.RunID
		cfg.Spend.Add(rep.Model, rep.Usage)
		plan = strings.TrimSpace(pr.Plan)
	}
	switch {
	case err != nil:
		rep.Skipped = "plan skipped (stage fails open): " + err.Error()
	case plan == "":
		rep.Skipped = "plan skipped: the planner returned an empty plan"
	}
	if rep.Skipped != "" {
		cfg.Obs.Note(rep.Skipped)
		if po != nil {
			po.PlanDone("", rep.Skipped)
		}
		return cfg.Task, rep
	}
	rep.Plan = plan
	cfg.Obs.Note("plan: ready — handing it to the solver")
	if po != nil {
		po.PlanDone(plan, "")
	}
	return cfg.Task + planHeader + plan, rep
}
