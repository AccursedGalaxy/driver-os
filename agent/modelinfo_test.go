package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestContextEstimateNoteOnce(t *testing.T) {
	obs := &recordObserver{}
	model := &scripted{replies: []string{"list_dir .", "answer done"}}
	_, err := runT(context.Background(), Config{
		Model:         model,
		Sandbox:       sbWith(t, nil),
		Task:          strings.Repeat("x", 300),
		Obs:           obs,
		ModelInfo:     llm.ModelInfo{ContextWindow: 10},
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, note := range obs.notes {
		if strings.HasPrefix(note, "context estimate ~") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("context estimate notes = %d, want 1; notes=%v", count, obs.notes)
	}
}

func TestUnknownModelInfoEmitsNoContextEstimate(t *testing.T) {
	obs := &recordObserver{}
	model := &scripted{replies: []string{"answer done"}}
	_, err := runT(context.Background(), Config{
		Model: model, Sandbox: sbWith(t, nil), Task: strings.Repeat("x", 300), Obs: obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range obs.notes {
		if strings.HasPrefix(note, "context estimate ~") {
			t.Fatalf("unexpected context estimate note: %q", note)
		}
	}
}
