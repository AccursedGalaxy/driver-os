package agent

import (
	"context"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// diagHint is a substring of diagnosticsMessage — its presence in a recorded request
// proves the code-intel slice-1 diagnostics feed surfaced a broken build.
const diagHint = "do not build yet"

func TestRunDiagnosticsFiresWhenStuck(t *testing.T) {
	// Text loop: two edits without an intervening green build reach DiagnoseAfterEdits,
	// so the diagnostics SOURCE runs ("exit 1" → red) and its errors are surfaced into the
	// observation the model reads next turn. This is the glm-5 case — edits, never checks,
	// never learns the build is broken.
	sp := &scripted{replies: []string{
		"write_file a.go x", // edit 1
		"write_file b.go y", // edit 2 → editsSinceGreen == threshold → diagnose, surfaced
		"answer done",       // the request for this turn carries the augmented observation
	}}
	res, err := Run(context.Background(), Config{
		Model:              sp,
		Sandbox:            sbWith(t, nil),
		Task:               "t",
		MaxIterations:      6,
		DiagnoseCmd:        "exit 1",
		DiagnoseAfterEdits: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !nudgeInjected(sp.calls, diagHint) {
		t.Fatalf("expected diagnostics surfaced when stuck with a broken build; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
}

func TestRunDiagnosticsOffByDefault(t *testing.T) {
	// DiagnoseAfterEdits unset (0) disarms the feed even with DiagnoseCmd present —
	// matching the opt-in default of the other lever knobs.
	sp := &scripted{replies: []string{
		"write_file a.go x", "write_file b.go y", "write_file c.go z", "answer done",
	}}
	_, err := Run(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, nil),
		Task:          "t",
		MaxIterations: 6,
		DiagnoseCmd:   "exit 1", // command set, but threshold 0 → off
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, diagHint) {
		t.Fatal("diagnostics fired with DiagnoseAfterEdits=0 (must be opt-in)")
	}
}

func TestRunDiagnosticsStaysOutBelowThreshold(t *testing.T) {
	// The multi-file objection: a change spanning several files is legitimately red
	// mid-flight. With only 2 edits under a threshold of 3, the feed stays silent — it does
	// NOT nag a model still assembling a multi-file change.
	sp := &scripted{replies: []string{
		"write_file a.go x", "write_file b.go y", "answer done",
	}}
	_, err := Run(context.Background(), Config{
		Model:              sp,
		Sandbox:            sbWith(t, nil),
		Task:               "t",
		MaxIterations:      6,
		DiagnoseCmd:        "exit 1",
		DiagnoseAfterEdits: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, diagHint) {
		t.Fatal("diagnostics fired below the edit threshold (would nag a mid-flight multi-file change)")
	}
}

func TestRunDiagnosticsSilentWhenBuildClean(t *testing.T) {
	// Past the threshold, but the build actually passes: the SOURCE runs, reports clean,
	// nothing is surfaced (and the stuck counter resets). Proves the feed checks real state
	// rather than nagging on edit-count alone.
	sp := &scripted{replies: []string{
		"write_file a.go x", "write_file b.go y", "answer done",
	}}
	_, err := Run(context.Background(), Config{
		Model:              sp,
		Sandbox:            sbWith(t, nil),
		Task:               "t",
		MaxIterations:      6,
		DiagnoseCmd:        "true", // exit 0 → clean
		DiagnoseAfterEdits: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, diagHint) {
		t.Fatal("diagnostics fired even though the build was clean")
	}
}

func TestRunDiagnosticsResetsOnGreenRun(t *testing.T) {
	// A green `run` resets the stuck counter, so the next lone edit is below threshold
	// again — the same reset that lets a model which verifies as it goes work in peace.
	sp := &scripted{replies: []string{
		"write_file a.go x", // edits-since-green = 1
		"run echo hi",       // green run → reset to 0
		"write_file b.go y", // edits-since-green = 1 (< 2) → no diagnose
		"answer done",
	}}
	_, err := Run(context.Background(), Config{
		Model:              sp,
		Sandbox:            sbWith(t, nil),
		Task:               "t",
		MaxIterations:      6,
		DiagnoseCmd:        "exit 1",
		DiagnoseAfterEdits: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nudgeInjected(sp.calls, diagHint) {
		t.Fatal("diagnostics fired after a green run reset the stuck counter")
	}
}

func TestRunNativeDiagnosticsFiresWhenStuck(t *testing.T) {
	// Native loop: the same stuck trajectory drives the feed to append its standalone
	// diagnostics message as a user turn after the turn's tool results.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "a.go", "content": "x"})},
		{structuredCall("c2", "write_file", map[string]any{"path": "b.go", "content": "y"})}, // → diagnose
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model:              ns,
		Sandbox:            sbWith(t, nil),
		Task:               "t",
		MaxIterations:      6,
		DiagnoseCmd:        "exit 1",
		DiagnoseAfterEdits: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !nudgeInjected(ns.calls, diagHint) {
		t.Fatalf("expected native diagnostics surfaced when stuck; outcome=%q reason=%q", res.Outcome, res.Reason)
	}
}
