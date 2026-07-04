package eval

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestCellCostAggregation(t *testing.T) {
	c := Cell{Case: "c", Model: "m", Trials: []Trial{
		{Cost: 0.10, Priced: true},
		{Cost: 0.30, Priced: true},
		{Cost: 0.20, Priced: true},
	}}
	if total, ok := c.CostTotal(); !ok || math.Abs(total-0.60) > 1e-9 {
		t.Errorf("CostTotal = %v (ok=%v), want 0.60", total, ok)
	}
	if p50, ok := c.CostP(50); !ok || math.Abs(p50-0.20) > 1e-9 {
		t.Errorf("CostP(50) = %v (ok=%v), want 0.20 (nearest-rank median)", p50, ok)
	}
}

func TestCellCostUnpricedReportsNotOK(t *testing.T) {
	// Trials ran but the model was unpriced — cost must be reported absent (ok=false),
	// never a misleading $0 that would make it look like the cheapest model.
	c := Cell{Case: "c", Model: "m", Trials: []Trial{{Priced: false}, {Priced: false}}}
	if _, ok := c.CostTotal(); ok {
		t.Error("CostTotal ok=true for an all-unpriced cell, want false")
	}
	if _, ok := c.CostP(50); ok {
		t.Error("CostP ok=true for an all-unpriced cell, want false")
	}
}

func TestWriteFilesArchivesTraces(t *testing.T) {
	dir := t.TempDir()
	r := &Report{Cells: []Cell{{
		Case: "calc", Model: "openai/gpt-5.5", Trials: []Trial{
			{Case: "calc", Model: "openai/gpt-5.5", Index: 1, Outcome: agent.Answered, Pass: true,
				Steps: []agent.Step{{Iter: 1, Verb: "run", Arg: "go test ./..."}}},
			{Case: "calc", Model: "openai/gpt-5.5", Index: 2, // a pre-first-turn provider error: no steps, no trace file.
				Outcome: agent.ProviderErr},
		},
	}}}
	if err := r.WriteFiles(dir); err != nil {
		t.Fatal(err)
	}
	// The compact report.json must NOT carry the steps (Trial.Steps is json:"-").
	rj, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rj), "go test ./...") {
		t.Error("report.json leaked the Step trace; it should stay compact (json:\"-\")")
	}
	// The trace with steps is archived under traces/; the step-less trial is skipped.
	traced := filepath.Join(dir, "traces", "calc__openai_gpt-5.5__t1.json")
	if b, err := os.ReadFile(traced); err != nil {
		t.Fatalf("expected trace file %s: %v", traced, err)
	} else if !strings.Contains(string(b), "go test ./...") {
		t.Errorf("trace file missing the step content:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "traces", "calc__openai_gpt-5.5__t2.json")); !os.IsNotExist(err) {
		t.Error("a step-less trial should NOT produce a trace file")
	}
}

// cellWith builds a synthetic cell so the metric math is tested without running
// the loop. Each trial is (outcome, pass, iters).
func cellWith(specs ...struct {
	outcome agent.Outcome
	pass    bool
	iters   int
}) Cell {
	c := Cell{Case: "c", Model: "m"}
	for i, s := range specs {
		c.Trials = append(c.Trials, Trial{
			Index:   i + 1,
			Outcome: s.outcome,
			Pass:    s.pass,
			Iters:   s.iters,
			Usage:   llm.Usage{PromptTokens: 1000 * (i + 1)},
		})
	}
	return c
}

func TestCellPassAndFalsePositiveRates(t *testing.T) {
	c := cellWith(
		struct {
			outcome agent.Outcome
			pass    bool
			iters   int
		}{agent.Answered, true, 6},
		struct {
			outcome agent.Outcome
			pass    bool
			iters   int
		}{agent.Answered, false, 5}, // answered but oracle-failed => false positive
		struct {
			outcome agent.Outcome
			pass    bool
			iters   int
		}{agent.HitCap, false, 20},
		struct {
			outcome agent.Outcome
			pass    bool
			iters   int
		}{agent.Answered, true, 8},
	)
	if c.Passes() != 2 || c.PassRate() != 0.5 {
		t.Errorf("Passes=%d PassRate=%.2f, want 2 and 0.50", c.Passes(), c.PassRate())
	}
	if c.FalsePositives() != 1 || c.FalsePositiveRate() != 0.25 {
		t.Errorf("FalsePositives=%d rate=%.2f, want 1 and 0.25", c.FalsePositives(), c.FalsePositiveRate())
	}
	// Convergence is over PASSING trials only (6 and 8), so p50 must not be the
	// hit-cap run's 20.
	if got := c.ItersP(50, true); got != 6 && got != 8 {
		t.Errorf("ItersP(50,pass-only)=%d, want a passing-trial value (6 or 8)", got)
	}
	h := c.Outcomes()
	if h[agent.Answered] != 3 || h[agent.HitCap] != 1 {
		t.Errorf("Outcomes = %v, want answered=3 hit_cap=1", h)
	}
}

func TestWilsonInterval(t *testing.T) {
	lo, hi := wilson(7, 10)
	if math.Abs(lo-0.397) > 0.02 || math.Abs(hi-0.892) > 0.02 {
		t.Errorf("wilson(7,10) = [%.3f, %.3f], want ≈[0.40, 0.89]", lo, hi)
	}
	// Degenerate cases stay in [0,1].
	if lo, hi := wilson(0, 0); lo != 0 || hi != 0 {
		t.Errorf("wilson(0,0) = [%.2f,%.2f], want [0,0]", lo, hi)
	}
	if lo, hi := wilson(5, 5); lo < 0 || hi > 1 {
		t.Errorf("wilson(5,5) = [%.2f,%.2f], must stay within [0,1]", lo, hi)
	}
}

func TestCellInfraTracking(t *testing.T) {
	c := Cell{Case: "c", Model: "m", Trials: []Trial{
		{Pass: true},
		{Pass: false, Err: "provider down"},
		{Pass: false},
	}}
	if c.Infra() != 1 {
		t.Errorf("Infra() = %d, want 1", c.Infra())
	}
	if c.PassRate() != 1.0/3.0 {
		t.Errorf("PassRate() = %v, want 1/3", c.PassRate())
	}
	if c.PassRateInfraExcluded() != 0.5 {
		t.Errorf("PassRateInfraExcluded() = %v, want 0.5", c.PassRateInfraExcluded())
	}

	// FIX 2: trial with Err != "" AND Pass == true must not inflate the rate.
	c2 := Cell{Case: "c", Model: "m", Trials: []Trial{
		{Pass: true, Err: "infra error"}, // infra error but oracle says pass
		{Pass: true, Err: ""},            // clean pass
		{Pass: false, Err: ""},           // clean fail
	}}
	// N=3, Infra=1. Denominator = 3-1 = 2.
	// Numerator should only count clean passes (Pass && Err == ""), so 1.
	// Rate = 1/2 = 0.5.
	// Old behavior would have counted both passes, Rate = 2/2 = 1.0.
	if got := c2.PassRateInfraExcluded(); got != 0.5 {
		t.Errorf("PassRateInfraExcluded with errored-but-passing trial = %v, want 0.5", got)
	}
}

func TestCellPlanCostAggregation(t *testing.T) {
	c := Cell{Case: "c", Model: "m", Trials: []Trial{
		{Cost: 0.10, Priced: true, PlanCost: 0.05, PlanPriced: true},
		{Cost: 0.20, Priced: true, ReviewCost: 0.02, ReviewPriced: true},
	}}
	// Total = (0.10 + 0.05) + (0.20 + 0.02) = 0.37
	if total, ok := c.CostTotal(); !ok || math.Abs(total-0.37) > 1e-9 {
		t.Errorf("CostTotal = %v (ok=%v), want 0.37", total, ok)
	}
}

func TestPercentileInt(t *testing.T) {
	xs := []int{20, 6, 8} // sorted: 6,8,20
	if got := percentileInt(xs, 50); got != 8 {
		t.Errorf("p50 = %d, want 8", got)
	}
	if got := percentileInt(xs, 100); got != 20 {
		t.Errorf("p100 = %d, want 20", got)
	}
	if got := percentileInt(nil, 50); got != 0 {
		t.Errorf("p50 of empty = %d, want 0", got)
	}
}

func TestMarkdownRendersCellRow(t *testing.T) {
	r := &Report{
		Manifest: Manifest{TrialsPer: 2},
		Cells: []Cell{cellWith(
			struct {
				outcome agent.Outcome
				pass    bool
				iters   int
			}{agent.Answered, true, 6},
			struct {
				outcome agent.Outcome
				pass    bool
				iters   int
			}{agent.Answered, false, 5},
		)},
	}
	md := r.Markdown()
	if !strings.Contains(md, "## c") {
		t.Errorf("markdown missing case header:\n%s", md)
	}
	if !strings.Contains(md, "⚠") { // the false positive should be flagged
		t.Errorf("markdown should flag the false positive:\n%s", md)
	}
}

func TestWriteFilesPersistsReview(t *testing.T) {
	dir := t.TempDir()
	rev := &agent.ReviewReport{Rounds: 2, Blocked: true, ReviewerModel: "openai/gpt-5.5",
		Usage: llm.Usage{PromptTokens: 1000, CompletionTokens: 100}}
	r := &Report{Cells: []Cell{{
		Case: "calc", Model: "deepseek/deepseek-v4-flash", Trials: []Trial{
			{Case: "calc", Model: "deepseek/deepseek-v4-flash", Index: 1, Outcome: agent.Unverified,
				Review: rev,
				Steps:  []agent.Step{{Iter: 1, Verb: "run", Arg: "go test ./..."}}},
		},
	}}}
	if err := r.WriteFiles(dir); err != nil {
		t.Fatal(err)
	}
	// report.json carries the compact review report (rounds/blocked/usage)…
	rj, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rj), `"blocked": true`) {
		t.Errorf("report.json should carry the review report:\n%s", rj)
	}
	// …and so does the archived trace, so a trial's review is replayable in place.
	tb, err := os.ReadFile(filepath.Join(dir, "traces", "calc__deepseek_deepseek-v4-flash__t1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tb), `"rounds": 2`) {
		t.Errorf("trace file should carry the review report:\n%s", tb)
	}
}

func TestCostTotalIncludesReviewerCost(t *testing.T) {
	c := Cell{Case: "c", Model: "m", Trials: []Trial{
		{Cost: 0.10, Priced: true, ReviewCost: 0.25, ReviewPriced: true},
		{Cost: 0.30, Priced: true}, // review off on this trial
	}}
	if total, ok := c.CostTotal(); !ok || math.Abs(total-0.65) > 1e-9 {
		t.Errorf("CostTotal = %v (ok=%v), want 0.65 (solver + reviewer)", total, ok)
	}
	// A cell where ONLY the reviewer was priced still reports a cost (the bill
	// is real even when the solver model has no Pricing entry).
	c2 := Cell{Trials: []Trial{{ReviewCost: 0.40, ReviewPriced: true}}}
	if total, ok := c2.CostTotal(); !ok || math.Abs(total-0.40) > 1e-9 {
		t.Errorf("CostTotal (reviewer-only) = %v (ok=%v), want 0.40", total, ok)
	}
}

func TestMarkdownRendersReviewGateSection(t *testing.T) {
	rev := &agent.ReviewReport{Rounds: 2, Blocked: true, ReviewerModel: "openai/gpt-5.5",
		Findings: []agent.ReviewedFinding{{Fate: agent.FateRepaired}, {Fate: agent.FateNote}}}
	r := &Report{Cells: []Cell{
		{Case: "calc", Model: "deepseek/deepseek-v4-flash", Trials: []Trial{
			{Outcome: agent.Unverified, Review: rev},
			{Outcome: agent.Answered}, // review off: must not break aggregation
		}},
	}}
	md := r.Markdown()
	if !strings.Contains(md, "## Review gate") {
		t.Fatalf("markdown should render a review-gate section when any trial carries one:\n%s", md)
	}
	if !strings.Contains(md, "openai/gpt-5.5") || !strings.Contains(md, "repaired=1") {
		t.Errorf("review-gate section should aggregate finding fates per reviewer:\n%s", md)
	}
	// No review anywhere -> no section.
	r2 := &Report{Cells: []Cell{{Trials: []Trial{{Outcome: agent.Answered}}}}}
	if strings.Contains(r2.Markdown(), "## Review gate") {
		t.Error("review-gate section must be absent when the gate never ran")
	}
}

func TestMarkdownRendersLadderMetrics(t *testing.T) {
	// Two trials in the same cell: one escalated (rung > 1), one not.
	r := &Report{
		Manifest: Manifest{TrialsPer: 2},
		Cells: []Cell{{
			Case:  "selfhist",
			Model: "ladder:abc123",
			Trials: []Trial{
				{
					Outcome: agent.Answered,
					Pass:    true,
					Ladder:  &TrialLadder{Attempts: 2, WinnerRung: 2, Escalated: true, Cost: 1.23, Priced: true},
				},
				{
					Outcome: agent.Answered,
					Pass:    true,
					Ladder:  &TrialLadder{Attempts: 1, WinnerRung: 1, Escalated: false, Cost: 0.45, Priced: true},
				},
			},
		}},
	}
	md := r.Markdown()
	if !strings.Contains(md, "escalation") {
		t.Fatalf("markdown should include escalation column when ladder data present:\n%s", md)
	}
	if !strings.Contains(md, "attempts mean") {
		t.Errorf("markdown should include attempts mean column:\n%s", md)
	}
	// Escalation column: 1/2 for one escalation out of two.
	if !strings.Contains(md, "1/2") {
		t.Errorf("markdown should show escalation fraction 1/2:\n%s", md)
	}
	// Mean attempts: (2+1)/2 = 1.50.
	if !strings.Contains(md, "1.50") {
		t.Errorf("markdown should show mean attempts 1.50:\n%s", md)
	}
	// Non-ladder cell: no ladder columns.
	r2 := &Report{
		Cells: []Cell{{
			Case:  "calc",
			Model: "openai/gpt-5.5",
			Trials: []Trial{
				{Outcome: agent.Answered, Pass: true},
			},
		}},
	}
	md2 := r2.Markdown()
	if strings.Contains(md2, "escalation") {
		t.Error("markdown must NOT render escalation column when no ladder data")
	}
}

func TestCellLadderCostByModel(t *testing.T) {
	// Two ladder trials: the reviewer model (gpt-5.5) appears on both trials,
	// the cheap rung-1 model (deepseek-v4-flash) appears on both, and the
	// expensive rung-2 model (claude-opus-4.8) only on the escalated one.
	cell := Cell{
		Case:  "selfhist",
		Model: "ladder:abc123",
		Trials: []Trial{
			{
				Outcome: agent.Answered,
				Pass:    true,
				Ladder: &TrialLadder{
					Attempts:   1,
					WinnerRung: 1,
					Escalated:  false,
					Cost:       1.15,
					Priced:     true,
					CostByModel: map[string]float64{
						"deepseek/deepseek-v4-flash": 0.61,
						"openai/gpt-5.5":            0.54,
					},
				},
			},
			{
				Outcome: agent.Answered,
				Pass:    true,
				Ladder: &TrialLadder{
					Attempts:   2,
					WinnerRung: 2,
					Escalated:  true,
					Cost:       3.32,
					Priced:     true,
					CostByModel: map[string]float64{
						"deepseek/deepseek-v4-flash":     0.61,
						"anthropic/claude-opus-4.8":      1.50,
						"openai/gpt-5.5":                 1.21,
					},
				},
			},
		},
	}

	// Per-model totals.
	byModel := cell.LadderCostByModel()
	if byModel == nil {
		t.Fatal("LadderCostByModel returned nil")
	}
	if math.Abs(byModel["deepseek/deepseek-v4-flash"]-1.22) > 1e-4 {
		t.Errorf("deepseek total = %v, want 1.22", byModel["deepseek/deepseek-v4-flash"])
	}
	if math.Abs(byModel["anthropic/claude-opus-4.8"]-1.50) > 1e-4 {
		t.Errorf("claude total = %v, want 1.50", byModel["anthropic/claude-opus-4.8"])
	}
	if math.Abs(byModel["openai/gpt-5.5"]-1.75) > 1e-4 {
		t.Errorf("gpt-5.5 total = %v, want 1.75", byModel["openai/gpt-5.5"])
	}

	// Touch rates: deepseek and gpt-5.5 on 2/2 = 100%, claude on 1/2 = 50%.
	touch := cell.LadderTouchRateByModel()
	if touch == nil {
		t.Fatal("LadderTouchRateByModel returned nil")
	}
	if math.Abs(touch["deepseek/deepseek-v4-flash"]-1.0) > 1e-4 {
		t.Errorf("deepseek touch = %v, want 1.0", touch["deepseek/deepseek-v4-flash"])
	}
	if math.Abs(touch["openai/gpt-5.5"]-1.0) > 1e-4 {
		t.Errorf("gpt-5.5 touch = %v, want 1.0", touch["openai/gpt-5.5"])
	}
	if math.Abs(touch["anthropic/claude-opus-4.8"]-0.50) > 1e-4 {
		t.Errorf("claude touch = %v, want 0.50", touch["anthropic/claude-opus-4.8"])
	}

	// Markdown must contain "$ by model" column header.
	rep := &Report{Manifest: Manifest{TrialsPer: 2}, Cells: []Cell{cell}}
	md := rep.Markdown()
	if !strings.Contains(md, "$ by model") {
		t.Fatalf("markdown missing $ by model column:\n%s", md)
	}
	// Check the breakdown line: it should mention all three models with
	// costs, shares, and touch rates.
	if !strings.Contains(md, "deepseek/deepseek-v4-flash") {
		t.Errorf("markdown missing deepseek model in $ by model:\n%s", md)
	}
	if !strings.Contains(md, "claude-opus-4.8") {
		t.Errorf("markdown missing claude model in $ by model:\n%s", md)
	}
	if !strings.Contains(md, "openai/gpt-5.5") {
		t.Errorf("markdown missing gpt-5.5 model in $ by model:\n%s", md)
	}
	// Total cost across models: 1.22+1.50+1.75 = 4.47.
	// Claude share: 1.50/4.47 ≈ 34%, gpt-5.5 share: 1.75/4.47 ≈ 39%.
	if !strings.Contains(md, "34%") && !strings.Contains(md, "39%") {
		t.Errorf("markdown should contain share percentages; got:\n%s", md)
	}
}
