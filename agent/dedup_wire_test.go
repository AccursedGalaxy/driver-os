package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestRunNativeWireDedupStubsRepeatObservations(t *testing.T) {
	// Build a long pytest-style observation string (>=300 chars)
	longObs := strings.Repeat("FAILED tests/test_spectree.py::test_something - assert 1 == 2\n", 10)
	if len(longObs) < obsDedupMinLen {
		t.Fatalf("longObs too short: %d", len(longObs))
	}

	// 5 identical turns, then answer.
	turns := repeatedStableRunTurns(5)
	turns = append(turns, []llm.ContentPart{llm.Text("banked before kill")})

	ns := &nativeScript{turns: turns}
	sb := sbWith(t, nil)
	res, err := RunNative(context.Background(), Config{
		Model:         ns,
		Sandbox:       sb,
		Tools:         stableRunTools(sb, longObs),
		Task:          "test task",
		MaxIterations: 10,
	})
	if err != nil {
		t.Fatalf("RunNative error: %v", err)
	}

	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered", res.Outcome, res.Reason)
	}

	// Assert escalating nudges 3, 4, 5 appear in the final request.
	lastReq := ns.calls[len(ns.calls)-1]
	for _, count := range []int{3, 4, 5} {
		nudge := escalatingRepeatNudge(count)
		if !requestTextContains(lastReq, nudge) {
			t.Errorf("nudge %d missing from last request", count)
		}
	}

	// Assert the wire was actually deduped.
	stubFound := false
	for _, req := range ns.calls {
		if requestTextContains(req, "— deduped]") {
			stubFound = true
			break
		}
	}
	if !stubFound {
		t.Errorf("dedup stub never found on the wire")
	}

	// Only iteration 0's observation is sent in full; every later identical copy is
	// stubbed and stays stubbed in the carried-forward history. So the final request
	// (which re-sends the whole transcript) holds at most one literal copy of longObs.
	// Without dedup it would hold five.
	if count := strings.Count(fmt.Sprintf("%+v", lastReq), longObs); count > 1 {
		t.Errorf("longObs appears %d times in last request, want at most 1", count)
	}
}

func TestRunNativeWireDedupKeepsCountSixKill(t *testing.T) {
	longObs := strings.Repeat("FAILED tests/test_spectree.py::test_something - assert 1 == 2\n", 10)
	ns := &nativeScript{turns: repeatedStableRunTurns(6)}
	sb := sbWith(t, nil)
	res, err := RunNative(context.Background(), Config{
		Model:         ns,
		Sandbox:       sb,
		Tools:         stableRunTools(sb, longObs),
		Task:          "test task",
		MaxIterations: 10,
	})
	if err != nil {
		t.Fatalf("RunNative error: %v", err)
	}

	if res.Outcome != KilledRepeat {
		t.Fatalf("Outcome = %q, want KilledRepeat", res.Outcome)
	}
	if res.Iterations != 6 {
		t.Errorf("Iterations = %d, want 6", res.Iterations)
	}
}

func TestRunTextWireDedupStubsRepeatObservations(t *testing.T) {
	longObs := strings.Repeat("FAILED tests/test_spectree.py::test_something - assert 1 == 2\n", 10)

	// Text loop scripted model
	replies := []string{
		"run pytest",
		"run pytest",
		"answer done",
	}
	sp := &scripted{replies: replies}

	tools := map[string]Tool{
		"run": {
			Run: func(ctx context.Context, arg string) (string, error) {
				return longObs, nil
			},
		},
	}

	res, err := Run(context.Background(), Config{
		Model:   sp,
		Tools:   tools,
		Sandbox: sbWith(t, nil),
	})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q, want Answered", res.Outcome)
	}

	// Assert that a second identical long observation is present as a stub in res.Steps
	foundStub := false
	for _, step := range res.Steps {
		if strings.HasPrefix(step.Observation, "[identical to iter") {
			foundStub = true
			break
		}
	}
	if !foundStub {
		t.Errorf("did not find deduped stub in res.Steps")
	}
}
