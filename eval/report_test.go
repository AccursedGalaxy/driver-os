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
