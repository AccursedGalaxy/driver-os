package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// These tests pin the frontier/state-aware explore-spiral policy (spiralState):
// productive top-down orientation of a large repo is never killed before its
// first useful read, while true navigation cycles and endless wandering still
// die deterministically. They model the opa-8781 P1 false-kill without importing
// any real repository.

// (deliverable 6a) Productive deep orientation — a long burst of discovery turns
// each hitting a STRICTLY NOVEL target (list_dir descending the tree, then
// searches with fresh queries) — must reach its first read_file and answer, not
// die at the old fixed window. Native protocol.
func TestRunNativeDeepOrientationNotKilledBeforeFirstRead(t *testing.T) {
	files := map[string]string{"a/b/c/f.txt": "x\n"}
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "."})},
		{structuredCall("2", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("3", "list_dir", map[string]any{"path": "a/b"})},
		{structuredCall("4", "list_dir", map[string]any{"path": "a/b/c"})},
		{structuredCall("5", "search", map[string]any{"pattern": "alpha"})},
		{structuredCall("6", "search", map[string]any{"pattern": "beta"})},
		{structuredCall("7", "search", map[string]any{"pattern": "gamma"})},
		{structuredCall("8", "search", map[string]any{"pattern": "delta"})},
		{structuredCall("9", "read_file", map[string]any{"path": "a/b/c/f.txt"})},
		{llm.Text("found it")},
	}
	ns := &nativeScript{turns: turns}
	res, err := runNativeT(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered — 8 strictly-novel discovery turns are orientation, not a spiral", res.Outcome, res.Reason)
	}
}

// (deliverable 6a) Same productive deep orientation, text protocol.
func TestRunDeepOrientationNotKilledBeforeFirstRead(t *testing.T) {
	sp := &scripted{replies: []string{
		"list_dir .", "list_dir a", "list_dir a/b", "list_dir a/b/c",
		"search alpha", "search beta", "search gamma", "search delta",
		"read_file a/b/c/f.txt", "answer found it",
	}}
	res, err := runT(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, map[string]string{"a/b/c/f.txt": "x\n"}),
		Task: "t", MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered — a novel-target orientation burst must reach the read", res.Outcome, res.Reason)
	}
}

// (deliverable 6b) Discovery turns whose targets were ALL visited before die at
// the cycle window as KilledSpiral.
func TestRunNativeRepeatedFrontierDiesAtWindow(t *testing.T) {
	// Two novel listings (a, b) establish the frontier; then noProgressWindow
	// turns revisiting only seen targets trip the cycle counter. Alternating a/b
	// keeps every turn distinct so the tight-loop detector stays out.
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("2", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("3", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("4", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("5", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("6", "list_dir", map[string]any{"path": "b"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := runNativeT(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"a/x": "1", "b/x": "1"}), Task: "t", MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q (%s), want KilledSpiral", res.Outcome, res.Reason)
	}
	if got := len(res.Steps); got != 2+noProgressWindow {
		t.Errorf("killed after %d turns, want %d (2 novel + window revisits)", got, 2+noProgressWindow)
	}
}

// Equivalent path spellings of the SAME directory (a, ./a, a/, a/.) are one
// frontier target, not four novel ones — a true navigation cycle over re-spelled
// paths must die at the cycle window, not slip to the hard wandering bound.
// Native protocol.
func TestRunNativeEquivalentPathSpellingsAreOneTarget(t *testing.T) {
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},    // novel
		{structuredCall("2", "list_dir", map[string]any{"path": "b"})},    // novel
		{structuredCall("3", "list_dir", map[string]any{"path": "./a"})},  // revisits a
		{structuredCall("4", "list_dir", map[string]any{"path": "b/"})},   // revisits b
		{structuredCall("5", "list_dir", map[string]any{"path": "a/."})},  // revisits a
		{structuredCall("6", "list_dir", map[string]any{"path": "./b/"})}, // revisits b
	}
	ns := &nativeScript{turns: turns}
	res, err := runNativeT(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"a/x": "1", "b/x": "1"}), Task: "t", MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q (%s), want KilledSpiral — re-spelled paths are the same directory", res.Outcome, res.Reason)
	}
	if got := len(res.Steps); got != 2+noProgressWindow {
		t.Errorf("killed after %d turns, want %d — re-spellings must not be treated as novel", got, 2+noProgressWindow)
	}
}

// Same guard, text protocol: list_dir a, ./a, a/, a/. are one target.
func TestRunEquivalentPathSpellingsAreOneTarget(t *testing.T) {
	sp := &scripted{replies: []string{
		"list_dir a", "list_dir b", "list_dir ./a", "list_dir b/", "list_dir a/.", "list_dir ./b/",
	}}
	res, err := runT(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, map[string]string{"a/x": "1", "b/x": "1"}),
		Task: "t", MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q (%s), want KilledSpiral — re-spelled paths are the same directory", res.Outcome, res.Reason)
	}
	if got := res.Iterations; got != 2+noProgressWindow {
		t.Errorf("killed after %d turns, want %d — re-spellings must not be treated as novel", got, 2+noProgressWindow)
	}
}

// (deliverable 6c) Alternating cycles (list_dir X, search Y, list_dir X, search
// Y, …) die — no single verb repeats, but the frontier stops growing.
func TestRunAlternatingCycleDies(t *testing.T) {
	sp := &scripted{replies: []string{
		"list_dir x", "search y",
		"list_dir x", "search y",
		"list_dir x", "search y",
	}}
	res, err := runT(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, map[string]string{"x/f": "1"}),
		Task: "t", MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q (%s), want KilledSpiral — an alternating list_dir/search cycle is spinning", res.Outcome, res.Reason)
	}
}

// (deliverable 6e) A read_file after a long discovery burst resets the discovery
// state: the orientation budget starts fresh, so a NEW novel burst after the
// read is not killed as though it continued the first one.
func TestRunNativeFirstReadResetsDiscoveryState(t *testing.T) {
	files := map[string]string{"f0.txt": "x\n"}
	turns := [][]llm.ContentPart{
		// First burst: 3 novel listings, then a read (phase transition -> reset).
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("2", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("3", "list_dir", map[string]any{"path": "c"})},
		{structuredCall("4", "read_file", map[string]any{"path": "f0.txt"})},
		// Second burst: re-listing a/b/c now looks NOVEL again (frontier cleared),
		// so it is orientation, not a continuation of the first burst's cycle.
		{structuredCall("5", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("6", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("7", "list_dir", map[string]any{"path": "c"})},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := runNativeT(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered — a read resets the discovery state", res.Outcome, res.Reason)
	}
}

// (deliverable 6g) The killing turn carries the explicit harness Observation, so
// a killed run never leaves an empty turn that reads as a broken checkout.
func TestRunNativeSpiralKillRecordsObservation(t *testing.T) {
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("2", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("3", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("4", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("5", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("6", "list_dir", map[string]any{"path": "b"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := runNativeT(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"a/x": "1", "b/x": "1"}), Task: "t", MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q (%s), want KilledSpiral", res.Outcome, res.Reason)
	}
	last := res.Steps[len(res.Steps)-1]
	if !strings.Contains(last.Observation, "harness: run killed by explore-spiral detector") {
		t.Errorf("killing turn Observation = %q, want the explicit harness note", last.Observation)
	}
}

// (deliverable 6g, text loop) The text-loop spiral kill likewise records the
// explicit harness Observation on the killing step.
func TestRunSpiralKillRecordsObservation(t *testing.T) {
	sp := &scripted{replies: []string{
		"list_dir a", "list_dir b", "list_dir a", "list_dir b", "list_dir a", "list_dir b",
	}}
	res, err := runT(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, map[string]string{"a/x": "1", "b/x": "1"}),
		Task: "t", MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q (%s), want KilledSpiral", res.Outcome, res.Reason)
	}
	last := res.Steps[len(res.Steps)-1]
	if !strings.Contains(last.Observation, "harness: run killed by explore-spiral detector") {
		t.Errorf("killing turn Observation = %q, want the explicit harness note", last.Observation)
	}
}
