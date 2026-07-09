package agent

import (
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Repros for the 2026-07-09 P1 (backlog "review-gate quote-anchoring CENSORS
// absence-class blockers"): a reviewer finding whose point is that a required
// file DOES NOT EXIST cannot survive classify — quote anchoring reads the
// named file, gets not-exists, and drops the finding as "quoted file
// unreadable". The solver is never shown the highest-severity finding class.
// Observed live in run 20260709-205042-4c4eac73 (opus-4.8 blocker, confidence
// 9, dropped in transport; the solver repaired only the delivered blocker and
// the run died on the round cap).
//
// Desired semantics: absence-class findings — file named but absent, or the
// reviewer prompt's documented `file: "n/a"` convention (council/reviewer.go)
// — skip byte-anchoring and continue down the existing ladder (severity,
// repro escalation, confidence). Genuine read errors on EXISTING files stay
// drops.

// absenceSolverTurns scripts a solver that edits, answers, absorbs one
// feedback round, and answers again.
func absenceSolverTurns() [][]llm.ContentPart {
	return [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},
		{llm.Text("done after feedback")},
	}
}

// A blocker naming a nonexistent file, with a repro command that CONFIRMS the
// absence (test -f fails), must be delivered to the solver as a repair round —
// not silently dropped.
func TestReviewAbsenceFindingMissingFileDelivered(t *testing.T) {
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{{
			File:            "calc_required_test.go", // does not exist in the workspace
			Quote:           "required test file calc_required_test.go is entirely absent",
			Severity:        "blocker",
			Confidence:      9,
			FailureScenario: "the task explicitly requires calc_required_test.go; without it the change is unverified",
			ReproCmd:        "test -f calc_required_test.go",
		}},
		{}, // round 2: clean
	}}
	res := reviewedRun(t, rv, Config{Task: "fix calc and add calc_required_test.go"}, absenceSolverTurns())
	if res.Review == nil {
		t.Fatal("no review report")
	}
	for _, f := range res.Review.Findings {
		if f.File == "calc_required_test.go" && f.Fate == FateDropped {
			t.Fatalf("absence finding was transport-dropped (drop_why %q) — the solver never saw the missing-deliverable blocker", f.DropWhy)
		}
	}
	if len(rv.inputs) < 2 {
		t.Fatalf("reviewer called %d time(s), want >=2 — the absence blocker must trigger a repair round, not a silent clean pass", len(rv.inputs))
	}
}

// The reviewer prompt's documented escape hatch for absence findings —
// file: "n/a" — must also survive classify (today it drops as unreadable).
func TestReviewAbsenceFindingNAFileDelivered(t *testing.T) {
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{{
			File:            "n/a",
			Quote:           "the task requires a new calc_required_test.go; no corresponding change exists in the diff",
			Severity:        "blocker",
			Confidence:      9,
			FailureScenario: "required deliverable missing entirely",
		}},
		{}, // round 2: clean
	}}
	res := reviewedRun(t, rv, Config{Task: "fix calc and add calc_required_test.go"}, absenceSolverTurns())
	if res.Review == nil {
		t.Fatal("no review report")
	}
	for _, f := range res.Review.Findings {
		if f.File == "n/a" && f.Fate == FateDropped {
			t.Fatalf("file:\"n/a\" absence finding was transport-dropped (drop_why %q) despite the reviewer prompt documenting the convention", f.DropWhy)
		}
	}
	if len(rv.inputs) < 2 {
		t.Fatalf("reviewer called %d time(s), want >=2 — the n/a absence blocker must trigger a repair round", len(rv.inputs))
	}
}
