package eval

import (
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
)

func TestAggregateReviewFates(t *testing.T) {
	rec := func(model string, fates ...string) agent.RunRecord {
		rep := &agent.ReviewReport{ReviewerModel: model, Rounds: 1}
		for _, f := range fates {
			rep.Findings = append(rep.Findings, agent.ReviewedFinding{Fate: f})
		}
		return agent.RunRecord{Review: rep}
	}
	stats := AggregateReviewFates([]agent.RunRecord{
		rec("gpt-5.5", agent.FateRepaired, agent.FateNote),
		rec("gpt-5.5", agent.FateRefuted, agent.FateExpired, agent.FateDropped),
		rec("deepseek", agent.FateRepaired),
		{}, // gate off — skipped.
	})
	g := stats["gpt-5.5"]
	if g.Runs != 2 || g.Rounds != 2 || g.Repaired != 1 || g.Refuted != 1 || g.Expired != 1 || g.Notes != 1 || g.Dropped != 1 {
		t.Fatalf("gpt-5.5 stats = %+v", g)
	}
	if got := g.FPRate(); got < 0.66 || got > 0.67 { // (1+1)/3
		t.Fatalf("fp rate = %v, want 2/3", got)
	}
	if d := stats["deepseek"]; d.FPRate() != 0 || d.Blockers() != 1 {
		t.Fatalf("deepseek stats = %+v", d)
	}
	out := RenderReviewFates(stats)
	if !strings.Contains(out, "gpt-5.5: runs=2") || !strings.Contains(out, "fp_rate=0.67") {
		t.Fatalf("render = %q", out)
	}
}
