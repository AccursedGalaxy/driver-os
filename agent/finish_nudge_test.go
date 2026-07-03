package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// nudgeHint is the substring shared by both finishNudge constants — its presence in
// a recorded request proves HP-4's near-cap finisher injected the hint.
const nudgeHint = "the task may already be complete"

// nudgeInjected reports whether any recorded request carried the finish-nudge hint.
func nudgeInjected(reqs []llm.Request, hint string) bool {
	for _, r := range reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Text(), hint) {
				return true
			}
		}
	}
	return false
}

func TestRunFinishNudgeFiresNearCapWhenSettled(t *testing.T) {
	// HP-4 text loop: a green `run` then distinct read_file calls (no edits, no
	// repeats, not list_dir — so no other detector fires) that would otherwise spin to
	// the cap. With FinishNudgeWindow set, once within the window of the cap with a
	// green last run and stable files, the one-time hint is injected.
	sp := &scripted{replies: []string{
		"run echo hi", // green run -> lastRunPassed
		"read_file a", "read_file b", "read_file c", "read_file d",
	}}
	res, err := Run(context.Background(), Config{
		Model:             sp,
		Sandbox:           sbWith(t, map[string]string{"a": "1\n", "b": "2\n", "c": "3\n", "d": "4\n"}),
		Task:              "t",
		MaxIterations:     5,
		FinishNudgeWindow: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !nudgeInjected(sp.calls, nudgeHint) {
		t.Fatalf("expected the finish nudge to be injected near the cap; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
}

func TestRunFinishNudgeOffByDefault(t *testing.T) {
	// FinishNudgeWindow unset (0): the finisher is disabled even on the same settled,
	// near-cap trajectory — matching the opt-in default of the other nudge knobs.
	sp := &scripted{replies: []string{
		"run echo hi", "read_file a", "read_file b", "read_file c", "read_file d",
	}}
	_, err := Run(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, map[string]string{"a": "1\n", "b": "2\n", "c": "3\n", "d": "4\n"}),
		Task:          "t",
		MaxIterations: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, nudgeHint) {
		t.Fatal("finish nudge fired with FinishNudgeWindow=0 (must be opt-in)")
	}
}

func TestRunFinishNudgeStaysOutWhenRunRed(t *testing.T) {
	// The finisher gates on a GREEN run: with the most recent `run` still failing there
	// is no "build/test green" signal, so it must not manufacture a finish.
	sp := &scripted{replies: []string{
		"run exit 1", // red run -> lastRunPassed stays false
		"read_file a", "read_file b", "read_file c", "read_file d",
	}}
	_, err := Run(context.Background(), Config{
		Model:             sp,
		Sandbox:           sbWith(t, map[string]string{"a": "1\n", "b": "2\n", "c": "3\n", "d": "4\n"}),
		Task:              "t",
		MaxIterations:     5,
		FinishNudgeWindow: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, nudgeHint) {
		t.Fatal("finish nudge fired despite the last run being red (no green done-signal)")
	}
}

func TestRunFinishNudgeStaysOutWhileFilesChurn(t *testing.T) {
	// The finisher gates on STABLE files: a model still mutating files every turn is
	// mid-work, so i-lastEditIter never reaches the window and the hint stays out — even
	// with a green run earlier and the cap in sight.
	sp := &scripted{replies: []string{
		"run echo hi", // green run
		"write_file f.txt x", "write_file g.txt y", "write_file h.txt z", "write_file i.txt w",
	}}
	_, err := Run(context.Background(), Config{
		Model:             sp,
		Sandbox:           sbWith(t, nil),
		Task:              "t",
		MaxIterations:     5,
		FinishNudgeWindow: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, nudgeHint) {
		t.Fatal("finish nudge fired while files were still being mutated every turn")
	}
}

func TestRunNativeFinishNudgeFiresNearCapWhenSettled(t *testing.T) {
	// HP-4 native loop: the same settled-near-cap trajectory drives the finisher to
	// append its standalone hint as a user message after the turn's tool results.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo hi"})}, // green run
		{structuredCall("c2", "read_file", map[string]any{"path": "a"})},
		{structuredCall("c3", "read_file", map[string]any{"path": "b"})},
		{structuredCall("c4", "read_file", map[string]any{"path": "c"})},
		{structuredCall("c5", "read_file", map[string]any{"path": "d"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model:             ns,
		Sandbox:           sbWith(t, map[string]string{"a": "1\n", "b": "2\n", "c": "3\n", "d": "4\n"}),
		Task:              "t",
		MaxIterations:     5,
		FinishNudgeWindow: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !nudgeInjected(ns.calls, nudgeHint) {
		t.Fatalf("expected the native finish nudge to be injected near the cap; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
}

// ---- Green-repeat nudge: the model re-runs the same passing command ----

// greenRepeatHint is the substring that proves the green-repeat nudge was injected.
const greenRepeatHint = "re-running it gains nothing"

func TestRunGreenRepeatNudgeFiresAfterThreeIdenticalGreens(t *testing.T) {
	// Text loop: three identical `run echo hi` (all pass) interleaved with
	// read_file calls to avoid the exact-repeat detector (maxRepeats=2). The
	// read_file is not a mutation, so the green-repeat counter is NOT reset.
	green := "run echo hi"
	sp := &scripted{replies: []string{
		green,         // green 1
		"read_file a", // breaks the repeat detector, not a mutation
		green,         // green 2
		"read_file b",
		green,         // green 3 -> nudge fires here
		"answer done",
	}}
	res, err := Run(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, map[string]string{"a": "x\n", "b": "y\n"}),
		Task:          "t",
		MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !nudgeInjected(sp.calls, greenRepeatHint) {
		t.Fatalf("expected the green-repeat nudge to fire after 3 identical green runs; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q, want Answered (nudge is not a kill)", res.Outcome)
	}
}

func TestRunGreenRepeatNudgeStaysOutOnDifferentGreens(t *testing.T) {
	// Different green commands -> each resets the fingerprint, counter never
	// reaches 3 for the same command.
	sp := &scripted{replies: []string{
		"run echo hi",
		"read_file a",
		"run echo there", // different command -> fp resets to 1, not 2
		"answer done",
	}}
	res, err := Run(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, map[string]string{"a": "x\n"}),
		Task:          "t",
		MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, greenRepeatHint) {
		t.Fatalf("green-repeat nudge fired despite different green commands; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
}

func TestRunGreenRepeatNudgeStaysOutWithFileMutation(t *testing.T) {
	// A write_file between greens resets the counter — the world changed,
	// re-running is legitimate.
	green := "run echo hi"
	sp := &scripted{replies: []string{
		green,
		"write_file f.txt x", // mutation resets
		green,
		green,
		green,
		"answer done",
	}}
	res, err := Run(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, nil),
		Task:          "t",
		MaxIterations: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, greenRepeatHint) {
		t.Fatalf("green-repeat nudge fired despite a file mutation between greens; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
}

func TestRunGreenRepeatNudgeStaysOutOnFailingRun(t *testing.T) {
	// A failing run resets the counter; even after 3 more greens interleaved
	// with reads, the nudge fires because the counter restarts from 0. But
	// here we only do 2 greens after the failure — the nudge should stay out.
	green := "run echo hi"
	sp := &scripted{replies: []string{
		green,
		"read_file a",
		green,
		"read_file b",
		"run exit 1", // failure resets counter to 0
		"read_file c",
		green, // counter=1
		"read_file d",
		green, // counter=2 — not enough
		"answer done",
	}}
	res, err := Run(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, map[string]string{"a": "x\n", "b": "y\n", "c": "z\n", "d": "w\n"}),
		Task:          "t",
		MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, greenRepeatHint) {
		t.Fatalf("green-repeat nudge fired despite a failing run reset; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
}

func TestRunNativeGreenRepeatNudgeFiresAfterThreeIdenticalGreens(t *testing.T) {
	// Native loop: three identical `run echo hi` interleaved with read_file
	// calls to avoid the exact-repeat detector.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo hi"})}, // green 1
		{structuredCall("r1", "read_file", map[string]any{"path": "a"})},    // break repeat
		{structuredCall("c2", "run", map[string]any{"command": "echo hi"})}, // green 2
		{structuredCall("r2", "read_file", map[string]any{"path": "b"})},    // break repeat
		{structuredCall("c3", "run", map[string]any{"command": "echo hi"})}, // green 3 -> nudge
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model:         ns,
		Sandbox:       sbWith(t, map[string]string{"a": "x\n", "b": "y\n"}),
		Task:          "t",
		MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range res.Steps {
		if strings.Contains(s.Observation, greenRepeatHint) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected the native green-repeat nudge to fire after 3 identical green runs; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q, want Answered (nudge is not a kill)", res.Outcome)
	}
}

func TestRunNativeGreenRepeatNudgeStaysOutOnDifferentGreens(t *testing.T) {
	// Different run commands -> different fingerprints, counter never hits 3 for
	// the same one.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo hi"})},
		{structuredCall("r1", "read_file", map[string]any{"path": "a"})},
		{structuredCall("c2", "run", map[string]any{"command": "echo there"})}, // different
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model:         ns,
		Sandbox:       sbWith(t, map[string]string{"a": "x\n"}),
		Task:          "t",
		MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Steps {
		if strings.Contains(s.Observation, greenRepeatHint) {
			t.Fatalf("native green-repeat nudge fired despite different green commands; outcome=%q reason=%q", res.Outcome, res.Reason)
		}
	}
}

func TestRunNativeGreenRepeatNudgeStaysOutWithFileMutation(t *testing.T) {
	// A write_file between greens resets the counter. After the mutation,
	// only 2 greens follow — not enough to fire.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo hi"})},
		{structuredCall("r1", "read_file", map[string]any{"path": "a"})},
		{structuredCall("c2", "run", map[string]any{"command": "echo hi"})}, // count=2
		{structuredCall("w", "write_file", map[string]any{"path": "f.txt", "content": "x"})}, // mutation resets
		{structuredCall("c3", "run", map[string]any{"command": "echo hi"})}, // count=1
		{structuredCall("r2", "read_file", map[string]any{"path": "b"})},
		{structuredCall("c4", "run", map[string]any{"command": "echo hi"})}, // count=2 — not enough
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model:         ns,
		Sandbox:       sbWith(t, map[string]string{"a": "x\n", "b": "y\n"}),
		Task:          "t",
		MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Steps {
		if strings.Contains(s.Observation, greenRepeatHint) {
			t.Fatalf("native green-repeat nudge fired despite a file mutation between greens; outcome=%q reason=%q", res.Outcome, res.Reason)
		}
	}
}
