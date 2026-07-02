package agent

// Config.ReasoningEffort must ride on EVERY model call of a run (triad-plan
// slice 1) — before 2026-07-03 the knob did not exist and every measurement
// ran at provider-default effort.

import (
	"context"
	"iter"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// effortRecorder answers immediately and records the effort each request asked for.
type effortRecorder struct{ got []string }

func (r *effortRecorder) Name() string { return "effort-recorder" }
func (r *effortRecorder) Capabilities() llm.Capabilities {
	return llm.Capabilities{Tools: true}
}

func (r *effortRecorder) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("effort-recorder")
}

func (r *effortRecorder) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	r.got = append(r.got, req.ReasoningEffort)
	return &llm.Response{Content: []llm.ContentPart{llm.Text("done")}, FinishReason: llm.FinishStop}, nil
}

func TestReasoningEffortRidesEveryModelCall(t *testing.T) {
	rec := &effortRecorder{}
	if _, err := RunNative(context.Background(), Config{
		Model:           rec,
		Sandbox:         sbWith(t, nil),
		Task:            "say done",
		ReasoningEffort: "xhigh",
	}); err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if len(rec.got) == 0 {
		t.Fatal("no model calls recorded")
	}
	for i, e := range rec.got {
		if e != "xhigh" {
			t.Errorf("call %d effort = %q, want xhigh", i, e)
		}
	}
}

func TestReasoningEffortDefaultsUnset(t *testing.T) {
	rec := &effortRecorder{}
	if _, err := RunNative(context.Background(), Config{
		Model:   rec,
		Sandbox: sbWith(t, nil),
		Task:    "say done",
	}); err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	for i, e := range rec.got {
		if e != "" {
			t.Errorf("call %d effort = %q, want unset", i, e)
		}
	}
}
