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

func TestCostOfResolution(t *testing.T) {
	u := llm.Usage{
		PromptTokens:     10000,
		CompletionTokens: 5000,
		CachedTokens:     2000,
	}

	tests := []struct {
		model string
		want  string // key in Pricing to compare against
	}{
		{"anthropic:claude-opus-4.8", "anthropic/claude-opus-4.8"}, // colon->slash
		{"openai:gpt-5.5", "openai/gpt-5.5"},                       // colon->slash
		{"anthropic:claude-fable-5", "claude-fable-5"},             // colon->bare
		{"anthropic:claude-opus-4-8", "claude-opus-4-8"},           // native hyphen id
		{"anthropic/claude-opus-4.8", "anthropic/claude-opus-4.8"}, // verbatim slash
		{"openai/gpt-5.6-sol", "openai/gpt-5.6-sol"},
		{"openai/gpt-5.6-terra", "openai/gpt-5.6-terra"},
		{"openai/gpt-5.6-luna", "openai/gpt-5.6-luna"},
		{"openai/gpt-5.6-sol-pro", "openai/gpt-5.6-sol-pro"},
		{"openai/gpt-5.6-terra-pro", "openai/gpt-5.6-terra-pro"},
		{"openai/gpt-5.6-luna-pro", "openai/gpt-5.6-luna-pro"}}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gotCost, gotOk := CostOf(tt.model, u)
			if !gotOk {
				t.Errorf("CostOf(%q) ok=false, want true", tt.model)
				return
			}
			wantPrice, ok := Pricing[tt.want]
			if !ok {
				t.Fatalf("test error: Pricing[%q] not found", tt.want)
			}
			wantCost := wantPrice.Cost(u)
			if math.Abs(gotCost-wantCost) > 1e-9 {
				t.Errorf("CostOf(%q) = %v, want %v (from %q)", tt.model, gotCost, wantCost, tt.want)
			}
		})
	}

	t.Run("totally-unknown-model", func(t *testing.T) {
		cost, ok := CostOf("totally-unknown-model", u)
		if ok || cost != 0 {
			t.Errorf("CostOf(unknown) = %v, %v; want 0, false", cost, ok)
		}
	})
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

func TestCostOfCacheDiscount(t *testing.T) {
	// openai/gpt-5.5: InPerM 5.00, OutPerM 30.00, CacheReadPerM 0.50
	// Usage: 1M prompt, 800K cached, 0 completion.
	// Expected: (1M - 800K) * 5.00/1M + 800K * 0.50/1M = 200K * 5.00/1M + 800K * 0.50/1M
	//           = 0.2 * 5.00 + 0.8 * 0.50 = 1.00 + 0.40 = 1.40.
	u := llm.Usage{PromptTokens: 1_000_000, CachedTokens: 800_000, CompletionTokens: 0}
	cost, ok := CostOf("openai/gpt-5.5", u)
	if !ok {
		t.Fatal("openai/gpt-5.5 not found in Pricing")
	}
	want := 1.40
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("CostOf(openai/gpt-5.5) = %v, want %v", cost, want)
	}
}
