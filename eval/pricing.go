package eval

import "github.com/AccursedGalaxy/driver-os/llm"

// Price is a model's list price in USD per 1,000,000 tokens, split by prompt vs
// completion (the two bill at different rates). It is the cost axis the DOGFOOD
// bake-offs kept asking for: capability is only half the picture — "5/5 at what
// price?" is the other half, and it splits a field that pass-rate alone collapses
// (gemini-3.1-pro and opus-4.8 both 5/5, but one spends ~4× the tokens).
type Price struct {
	InPerM  float64 // USD per 1M prompt tokens.
	OutPerM float64 // USD per 1M completion tokens.
}

// Cost returns the USD cost of one trial's token usage at this price. Cached
// prompt tokens are billed at the full prompt rate here: Usage.CachedTokens is a
// subset of PromptTokens that is already counted once, and OpenRouter discounts
// it — so NOT subtracting it is a deliberate, documented over-estimate (a cost
// ceiling), never a silent under-count.
func (p Price) Cost(u llm.Usage) float64 {
	return float64(u.PromptTokens)/1e6*p.InPerM + float64(u.CompletionTokens)/1e6*p.OutPerM
}

// Pricing is the hand-maintained price table for the eval roster, USD per 1M
// tokens, transcribed from openrouter.ai/models on 2026-06-02. It is a STATIC,
// in-repo table by design, not a live fetch: a report is a git-pinned,
// reproducible artifact (see report.go's Manifest), so the cost column must be
// pinned too — a price that moved after a run must not silently rewrite a past
// report's numbers. Update this table when the roster or its prices change; a
// live source can later drop in behind CostOf without touching its callers.
//
// A slug absent here renders "—" (unknown), never $0 — free and unpriced are
// different facts, and conflating them would make an un-priced model look like
// the cheapest in the table.
var Pricing = map[string]Price{
	// flagships
	"openai/gpt-5.5":                {InPerM: 1.25, OutPerM: 10.00},
	"qwen/qwen3.7-max":              {InPerM: 1.20, OutPerM: 6.00},
	"anthropic/claude-opus-4.8":     {InPerM: 5.00, OutPerM: 25.00},
	"google/gemini-3.1-pro-preview": {InPerM: 1.25, OutPerM: 10.00},
	// affordable
	"deepseek/deepseek-v4-flash":   {InPerM: 0.10, OutPerM: 0.20},
	"tencent/hy3-preview":          {InPerM: 0.30, OutPerM: 1.20},
	"google/gemini-2.5-flash-lite": {InPerM: 0.10, OutPerM: 0.40},
	// coding
	"anthropic/claude-opus-4.7":     {InPerM: 5.00, OutPerM: 25.00},
	"moonshotai/kimi-k2.6":          {InPerM: 0.60, OutPerM: 2.50},
	"moonshotai/kimi-k2.6:free":     {InPerM: 0.00, OutPerM: 0.00}, // free tier: priced, and the price is zero.
	"google/gemini-3-flash-preview": {InPerM: 0.30, OutPerM: 2.50},
}

// CostOf returns the USD cost for a model's usage and whether the model was
// priced at all. ok=false means the slug is absent from Pricing — the caller
// renders "—", not 0, because free and unknown are different facts.
func CostOf(model string, u llm.Usage) (cost float64, ok bool) {
	p, ok := Pricing[model]
	if !ok {
		return 0, false
	}
	return p.Cost(u), true
}
