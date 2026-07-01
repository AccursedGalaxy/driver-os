package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Review #7 / HP-8: MaxTotalTokens makes cost a first-class cap. The scripted
// fakes report a fixed 15 total tokens per turn, so budgets translate directly
// into turn counts.

func TestRunHitsTokenBudget(t *testing.T) {
	files := map[string]string{"a": "1\n", "b": "2\n", "c": "3\n", "d": "4\n"}
	sp := &scripted{replies: []string{"read_file a", "read_file b", "read_file c", "read_file d"}}
	obs := &recordObserver{}
	res, err := Run(context.Background(), Config{
		Model:          sp,
		Sandbox:        sbWith(t, files),
		Obs:            obs,
		Task:           "t",
		MaxIterations:  10,
		MaxTotalTokens: 20, // turn 1 = 15 (< 20, keep going), turn 2 = 30 (>= 20, stop at next boundary).
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != HitBudget {
		t.Fatalf("outcome = %q, want hit_budget (reason %q)", res.Outcome, res.Reason)
	}
	if res.Iterations != 2 {
		t.Errorf("iterations = %d, want 2 (the crossing turn is still processed; the next is not paid for)", res.Iterations)
	}
	if !strings.Contains(res.Reason, "token budget") {
		t.Errorf("reason does not name the budget: %q", res.Reason)
	}
	var sawUsageNote bool
	for _, n := range obs.notes {
		if strings.Contains(n, "tokens:") && strings.Contains(n, "budget 20") {
			sawUsageNote = true
		}
	}
	if !sawUsageNote {
		t.Errorf("per-turn cumulative usage must flow through Obs.Note (what makes the cap tunable); notes=%v", obs.notes)
	}
}

func TestRunNativeHitsTokenBudget(t *testing.T) {
	files := map[string]string{"a": "1\n", "b": "2\n", "c": "3\n", "d": "4\n"}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "read_file", map[string]any{"path": "a"})},
		{structuredCall("c2", "read_file", map[string]any{"path": "b"})},
		{structuredCall("c3", "read_file", map[string]any{"path": "c"})},
		{structuredCall("c4", "read_file", map[string]any{"path": "d"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model:          ns,
		Sandbox:        sbWith(t, files),
		Task:           "t",
		MaxIterations:  10,
		MaxTotalTokens: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != HitBudget {
		t.Fatalf("native outcome = %q, want hit_budget (reason %q)", res.Outcome, res.Reason)
	}
	if res.Iterations != 2 {
		t.Errorf("native iterations = %d, want 2", res.Iterations)
	}
}

func TestRunAnswerOnCrossingTurnStillAnswers(t *testing.T) {
	// The turn that CROSSES the budget is still processed — it may be the answer.
	// Budget 16: turn 1 (15) is under, turn 2 runs (and answers) even though it
	// pushes the total to 30.
	sp := &scripted{replies: []string{"read_file a", "answer done"}}
	res, err := Run(context.Background(), Config{
		Model:          sp,
		Sandbox:        sbWith(t, map[string]string{"a": "1\n"}),
		Task:           "t",
		MaxIterations:  10,
		MaxTotalTokens: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %q, want answered — the crossing turn must not be discarded", res.Outcome)
	}
}
