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
