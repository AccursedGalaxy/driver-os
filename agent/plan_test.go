package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// plannerStub is the minimal Planner a plan-stage test needs: a canned result
// (or error) plus a record of what it was asked to plan.
type plannerStub struct {
	res *PlanResult
	err error
	in  []PlanInput
}

func (p *plannerStub) Plan(_ context.Context, in PlanInput) (*PlanResult, error) {
	p.in = append(p.in, in)
	return p.res, p.err
}

// The plan stage seeds the SOLVER's first message with the plan while keeping
// the ORIGINAL task on the record (RunResult.Task) — and the planner's usage
// stays OUT of the solver's bill, attributable per role.
func TestPlanStageSeedsTaskAndReport(t *testing.T) {
	stub := &plannerStub{res: &PlanResult{
		Plan:  "APPROACH — do the thing.",
		Model: "planner-model",
		Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		RunID: "plan-run-1",
	}}
	sp := &scripted{replies: []string{"answer done"}}
	res, err := runT(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Task:    "test task",
		Planner: stub,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stub.in) != 1 || stub.in[0].Task != "test task" || stub.in[0].Continuing {
		t.Fatalf("planner asked with %+v, want the original task, fresh conversation", stub.in)
	}
	first := sp.calls[0].Messages[0].Text()
	if !strings.Contains(first, "test task") || !strings.Contains(first, "APPROACH — do the thing.") {
		t.Errorf("first message = %q, want the task AND the plan", first)
	}
	if !strings.Contains(first, "IMPLEMENTATION PLAN") {
		t.Errorf("first message lacks the plan frame: %q", first)
	}
	if res.Task != "test task" {
		t.Errorf("RunResult.Task = %q, want the ORIGINAL task", res.Task)
	}
	if res.Plan == nil || res.Plan.Plan != "APPROACH — do the thing." || res.Plan.Model != "planner-model" || res.Plan.PlannerRun != "plan-run-1" {
		t.Errorf("Plan report = %+v", res.Plan)
	}
	// Planner usage must not leak into the solver's bill (per-role attribution).
	if res.Usage.PromptTokens != 10 || res.Plan.Usage.PromptTokens != 100 {
		t.Errorf("usage bleed: solver %+v, plan %+v", res.Usage, res.Plan.Usage)
	}
}

// A planner fault FAILS OPEN: the solver runs the untouched task and the skip
// reason (plus the failed attempt's usage) is recorded.
func TestPlanStageFailsOpen(t *testing.T) {
	stub := &plannerStub{
		res: &PlanResult{Model: "planner-model", Usage: llm.Usage{PromptTokens: 7}},
		err: errors.New("planner exploded"),
	}
	sp := &scripted{replies: []string{"answer done"}}
	res, err := runT(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Task:    "test task",
		Planner: stub,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q, want %q — a planner fault must never block", res.Outcome, Answered)
	}
	first := sp.calls[0].Messages[0].Text()
	if strings.Contains(first, "IMPLEMENTATION PLAN") {
		t.Errorf("failed plan still framed into the task: %q", first)
	}
	if res.Plan == nil || !strings.Contains(res.Plan.Skipped, "planner exploded") {
		t.Errorf("Plan report = %+v, want the skip reason", res.Plan)
	}
	if res.Plan.Usage.PromptTokens != 7 {
		t.Errorf("failed attempt's usage lost: %+v", res.Plan.Usage)
	}
}

// An empty plan is a skip, not a frame around nothing.
func TestPlanStageEmptyPlanSkips(t *testing.T) {
	stub := &plannerStub{res: &PlanResult{Plan: "  \n ", Model: "m"}}
	sp := &scripted{replies: []string{"answer done"}}
	res, err := runT(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Task:    "test task",
		Planner: stub,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(sp.calls[0].Messages[0].Text(), "IMPLEMENTATION PLAN") {
		t.Error("empty plan was framed into the task")
	}
	if res.Plan == nil || res.Plan.Skipped == "" {
		t.Errorf("Plan report = %+v, want an empty-plan skip", res.Plan)
	}
}

// No Planner = byte-identical no-op: nil report, untouched task.
func TestPlanStageOffIsInert(t *testing.T) {
	res, sp := runScript(t, map[string]string{"go.mod": "module x\n"}, []string{"answer ok"})
	if res.Plan != nil {
		t.Errorf("Plan = %+v with no Planner configured, want nil", res.Plan)
	}
	if got := sp.calls[0].Messages[0].Text(); strings.Contains(got, "IMPLEMENTATION PLAN") {
		t.Errorf("task framed without a planner: %q", got)
	}
}

// A continuing conversation (History) is flagged to the planner.
func TestPlanStageContinuingFlag(t *testing.T) {
	stub := &plannerStub{res: &PlanResult{Plan: "p", Model: "m"}}
	sp := &scripted{replies: []string{"answer done"}}
	_, err := runT(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Task:    "follow-up",
		History: []llm.Message{llm.User("TASK: earlier"), llm.Assistant("ok")},
		Planner: stub,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stub.in) != 1 || !stub.in[0].Continuing {
		t.Fatalf("planner input = %+v, want Continuing=true", stub.in)
	}
	// The plan-augmented input still arrives as the appended user turn.
	msgs := sp.calls[0].Messages
	last := msgs[len(msgs)-1].Text()
	if !strings.Contains(last, "follow-up") || !strings.Contains(last, "IMPLEMENTATION PLAN") {
		t.Errorf("continuing turn = %q, want task + plan", last)
	}
}

// SetPlanner must govern the NEXT Send — the /plan seam.
func TestSessionSetPlanner(t *testing.T) {
	var saw []Planner
	loop := func(_ context.Context, cfg Config) (*RunResult, error) {
		saw = append(saw, cfg.Planner)
		return &RunResult{Outcome: Answered}, nil
	}
	s := newSessionT2(Config{}, loop)
	if _, err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	stub := &plannerStub{}
	s.SetPlanner(stub)
	if s.Planner() != Planner(stub) {
		t.Fatalf("Planner() = %#v after SetPlanner", s.Planner())
	}
	if _, err := s.Send(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	s.SetPlanner(nil)
	if _, err := s.Send(context.Background(), "three"); err != nil {
		t.Fatal(err)
	}
	if saw[0] != nil || saw[1] != Planner(stub) || saw[2] != nil {
		t.Errorf("planner per Send = %#v, want [nil stub nil]", saw)
	}
}
