package eval

import (
	"math"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestPriceCost(t *testing.T) {
	p := Price{InPerM: 1.25, OutPerM: 10.00}
	// 200K prompt @ $1.25/M = $0.25; 50K completion @ $10/M = $0.50; total $0.75.
	got := p.Cost(llm.Usage{PromptTokens: 200_000, CompletionTokens: 50_000})
	if math.Abs(got-0.75) > 1e-9 {
		t.Errorf("Cost = %v, want 0.75", got)
	}
}

func TestCostOfDistinguishesUnpricedFromFree(t *testing.T) {
	u := llm.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}

	// A priced model returns a real number with ok=true.
	if cost, ok := CostOf("deepseek/deepseek-v4-flash", u); !ok || cost <= 0 {
		t.Errorf("priced model: cost=%v ok=%v, want a positive cost and ok", cost, ok)
	}
	// The free tier is PRICED at zero — ok=true, cost=0 (free is a fact, not unknown).
	if cost, ok := CostOf("moonshotai/kimi-k2.6:free", u); !ok || cost != 0 {
		t.Errorf("free model: cost=%v ok=%v, want 0 and ok=true", cost, ok)
	}
	// An unknown slug is NOT priced — ok=false (the report renders "—", not $0).
	if cost, ok := CostOf("acme/does-not-exist", u); ok || cost != 0 {
		t.Errorf("unknown model: cost=%v ok=%v, want 0 and ok=false", cost, ok)
	}
}
