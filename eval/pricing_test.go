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

	// CostOf("anthropic:claude-fable-5", u) resolves and prices correctly.
	if cost, ok := CostOf("anthropic:claude-fable-5", u); !ok || cost != 60.0 {
		// 1M prompt @ 10 + 1M completion @ 50 = 60.
		t.Errorf("prefixed model: cost=%v ok=%v, want 60.0 and ok=true", cost, ok)
	}

	// CostOf("moonshotai/kimi-k2.6:free", u) still resolves to $0 priced (regression).
	if cost, ok := CostOf("moonshotai/kimi-k2.6:free", u); !ok || cost != 0 {
		t.Errorf("unrecognized prefix: cost=%v ok=%v, want 0 and ok=true", cost, ok)
	}
}

func TestCacheDiscount(t *testing.T) {
	// 1M prompt tokens, 500K of which are cached.
	u := llm.Usage{PromptTokens: 1_000_000, CachedTokens: 500_000, CompletionTokens: 0}

	pNoCache := Price{InPerM: 10.0, OutPerM: 50.0}
	pWithCache := Price{InPerM: 10.0, OutPerM: 50.0, CacheReadPerM: 1.0}

	costNoCache := pNoCache.Cost(u)
	costWithCache := pWithCache.Cost(u)

	// Without cache pricing: 1M * 10 = 10.
	if costNoCache != 10.0 {
		t.Errorf("costNoCache = %v, want 10.0", costNoCache)
	}

	// With cache pricing: 500K * 10 + 500K * 1 = 5 + 0.5 = 5.5.
	if costWithCache != 5.5 {
		t.Errorf("costWithCache = %v, want 5.5", costWithCache)
	}

	if costWithCache >= costNoCache {
		t.Errorf("expected cache discount, but %v >= %v", costWithCache, costNoCache)
	}
}
