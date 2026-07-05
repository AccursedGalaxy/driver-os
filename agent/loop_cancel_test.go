package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestRunNativeCancel(t *testing.T) {
	// A script that would run forever if not cancelled.
	script := &nativeScript{
		turns: [][]llm.ContentPart{
			{toolCall("1", "read_file", "foo.txt")},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())

	// Case 1: Pre-cancelled context.
	cancel()
	res, err := RunNative(ctx, Config{
		Model:   script,
		Sandbox: sbWith(t, nil),
		Task:    "test cancel",
	})
	if err != nil {
		t.Fatalf("RunNative returned error: %v", err)
	}
	if res.Outcome != Canceled {
		t.Errorf("Outcome = %v, want %v", res.Outcome, Canceled)
	}
	if res.Iterations != 0 {
		t.Errorf("Iterations = %d, want 0", res.Iterations)
	}
	if res.Reason != "run canceled by the caller (interrupt)" {
		t.Errorf("Reason = %q, want %q", res.Reason, "run canceled by the caller (interrupt)")
	}
}

func TestRunTextCancel(t *testing.T) {
	script := &scripted{
		replies: []string{"read_file foo.txt"},
	}
	ctx, cancel := context.WithCancel(context.Background())

	// Case 1: Pre-cancelled context.
	cancel()
	res, err := Run(ctx, Config{
		Model:   script,
		Sandbox: sbWith(t, nil),
		Task:    "test cancel",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Outcome != Canceled {
		t.Errorf("Outcome = %v, want %v", res.Outcome, Canceled)
	}
	if res.Iterations != 0 {
		t.Errorf("Iterations = %d, want 0", res.Iterations)
	}
	if res.Reason != "run canceled by the caller (interrupt)" {
		t.Errorf("Reason = %q, want %q", res.Reason, "run canceled by the caller (interrupt)")
	}
}

func TestRunNativeCancelBetweenTurns(t *testing.T) {
	// A script that returns a tool call.
	script := &nativeScript{
		turns: [][]llm.ContentPart{
			{toolCall("1", "read_file", "foo.txt")},
			{toolCall("2", "read_file", "bar.txt")},
		},
	}

	// We want to cancel AFTER the first iteration completes but BEFORE the second starts.
	// We can use a custom Tool that cancels the context.
	ctx, cancel := context.WithCancel(context.Background())

	tools := map[string]Tool{
		"read_file": {
			RunJSON: func(c context.Context, _ json.RawMessage) (string, error) {
				cancel() // Cancel the outer context!
				return "file content", nil
			},
		},
	}

	res, err := RunNative(ctx, Config{
		Model:   script,
		Sandbox: sbWith(t, nil),
		Task:    "test cancel between turns",
		Tools:   tools,
	})
	if err != nil {
		t.Fatalf("RunNative returned error: %v", err)
	}

	// It should have finished 1 iteration and then noticed the cancellation at the top of the 2nd.
	if res.Outcome != Canceled {
		t.Errorf("Outcome = %v, want %v", res.Outcome, Canceled)
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", res.Iterations)
	}
}
