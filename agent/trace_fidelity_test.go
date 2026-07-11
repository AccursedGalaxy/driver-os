package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestRunStepRecordsAugmentedObservation(t *testing.T) {
	// Review #5: the text loop must record the observation AS AUGMENTED by the
	// diagnostics/churn/finish nudges — exactly what was fed back to the model —
	// not the pre-nudge draft. Otherwise RunResult.Steps and the persisted
	// transcript diverge from the conversation, and eval/dogfood replays debug
	// against text the model never saw.
	sp := &scripted{replies: []string{"run exit 1", "answer done"}}
	res, err := runT(context.Background(), Config{
		Model:          sp,
		Sandbox:        sbWith(t, nil),
		Task:           "t",
		MaxIterations:  4,
		ChurnNudgeRuns: 1, // the single failing run trips the churn nudge onto this observation.
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) == 0 {
		t.Fatal("no steps recorded")
	}
	step := res.Steps[0]
	if !strings.Contains(step.Observation, "[hint:") {
		t.Fatalf("recorded step is missing the churn nudge the model actually read:\n%s", step.Observation)
	}
	want := "OBSERVATION:\n" + step.Observation
	for _, m := range res.Messages {
		if m.Role == llm.RoleUser && m.Text() == want {
			return
		}
	}
	t.Fatalf("no fed-back user message matches the recorded step observation:\n%s", step.Observation)
}

func TestRunNativeToolTurnRecordsReply(t *testing.T) {
	// Review #6: a native tool turn often narrates intent/conclusions in prose
	// alongside its calls. That narration must land on the turn's first step —
	// the text loop records Reply on every step; losing it in the native loop
	// blinds eval/dogfood debugging to what the model SAID.
	turns := [][]llm.ContentPart{
		{llm.Text("Checking the layout first."), structuredCall("c1", "list_dir", map[string]any{"path": "."})},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := runNativeT(context.Background(), Config{
		Model:         ns,
		Sandbox:       sbWith(t, nil),
		Task:          "t",
		MaxIterations: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) == 0 {
		t.Fatal("no steps recorded")
	}
	first := res.Steps[0]
	if first.Verb != "list_dir" {
		t.Fatalf("unexpected first step %q", first.Verb)
	}
	if first.Reply != "Checking the layout first." {
		t.Errorf("tool-turn narration not recorded: Reply=%q", first.Reply)
	}
}
