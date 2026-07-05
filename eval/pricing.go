package eval

import (
	"strings"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Price is a model's list price in USD per 1,000,000 tokens, split by prompt vs
// completion (the two bill at different rates). It is the cost axis the DOGFOOD
// bake-offs kept asking for: capability is only half the picture — "5/5 at what
// price?" is the other half, and it splits a field that pass-rate alone collapses
// (gemini-3.1-pro and opus-4.8 both 5/5, but one spends ~4× the tokens).
type Price struct {
	InPerM        float64 // USD per 1M prompt tokens.
	OutPerM       float64 // USD per 1M completion tokens.
	CacheReadPerM float64 // USD per 1M cache-read prompt tokens.
}

// Cost returns the USD cost of one trial's token usage at this price.
//
// When CacheReadPerM > 0, Usage.CachedTokens (a subset of PromptTokens) are
// billed at that rate, and only the remainder at InPerM.
//
// When CacheReadPerM == 0, cached tokens are billed at the full prompt rate:
// Usage.CachedTokens is a subset of PromptTokens that is already counted once,
// and OpenRouter discounts it — so NOT subtracting it is a deliberate,
// documented over-estimate (a cost ceiling), never a silent under-count.
func (p Price) Cost(u llm.Usage) float64 {
	if p.CacheReadPerM > 0 {
		uncached := u.PromptTokens - u.CachedTokens
		if uncached < 0 {
			uncached = 0
		}
		return float64(uncached)/1e6*p.InPerM +
			float64(u.CachedTokens)/1e6*p.CacheReadPerM +
			float64(u.CompletionTokens)/1e6*p.OutPerM
	}
	return float64(u.PromptTokens)/1e6*p.InPerM + float64(u.CompletionTokens)/1e6*p.OutPerM
}

// Pricing is the hand-maintained price table for the eval roster, USD per 1M
// tokens, in/out + cache-read refreshed from live OpenRouter 2026-07-05. It is a STATIC,
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
	"openai/gpt-5.5":                {InPerM: 5.00, OutPerM: 30.00, CacheReadPerM: 0.50},
	"qwen/qwen3.7-max":              {InPerM: 1.20, OutPerM: 6.00},
	"anthropic/claude-opus-4.8":     {InPerM: 5.00, OutPerM: 25.00, CacheReadPerM: 0.50},
	"google/gemini-3.1-pro-preview": {InPerM: 2.00, OutPerM: 12.00, CacheReadPerM: 0.20},

	// Native-API-id rows (hyphen form) paralleling the OpenRouter dotted slugs.
	"claude-opus-4-8":  {InPerM: 5.00, OutPerM: 25.00, CacheReadPerM: 0.50},
	"claude-opus-4-7":  {InPerM: 5.00, OutPerM: 25.00, CacheReadPerM: 0.50},
	"claude-sonnet-5":  {InPerM: 3.00, OutPerM: 15.00},
	"claude-haiku-4-5": {InPerM: 1.00, OutPerM: 5.00, CacheReadPerM: 0.10},

	// affordable
	"deepseek/deepseek-v4-flash":   {InPerM: 0.09, OutPerM: 0.18, CacheReadPerM: 0.018},
	"tencent/hy3-preview":          {InPerM: 0.30, OutPerM: 1.20},
	"google/gemini-2.5-flash-lite": {InPerM: 0.10, OutPerM: 0.40},
	// coding
	"anthropic/claude-opus-4.7":     {InPerM: 5.00, OutPerM: 25.00, CacheReadPerM: 0.50},
	"moonshotai/kimi-k2.6":          {InPerM: 0.60, OutPerM: 2.50},
	"moonshotai/kimi-k2.6:free":     {InPerM: 0.00, OutPerM: 0.00}, // free tier: priced, and the price is zero.
	"google/gemini-3-flash-preview": {InPerM: 0.50, OutPerM: 3.00, CacheReadPerM: 0.05},

	// Round-12 candidates — untested families/tiers scouted against the live
	// catalog (2026-06-03) and run through the explore+code probe before any
	// promotion into `roster`. Grok is written "4.20" on OpenRouter (no "4.2").
	"x-ai/grok-4.3":               {InPerM: 1.25, OutPerM: 2.50},
	"x-ai/grok-4.20":              {InPerM: 1.25, OutPerM: 2.50},
	"z-ai/glm-5":                  {InPerM: 0.60, OutPerM: 1.92, CacheReadPerM: 0.12},
	"openai/gpt-5.4":              {InPerM: 2.50, OutPerM: 15.00},
	"openai/gpt-5.2-codex":        {InPerM: 1.75, OutPerM: 14.00},
	"anthropic/claude-sonnet-4.6": {InPerM: 3.00, OutPerM: 15.00},
	"claude-fable-5":              {InPerM: 10.00, OutPerM: 50.00, CacheReadPerM: 1.00},
	"qwen/qwen3-coder":            {InPerM: 0.22, OutPerM: 1.80},
	"minimax/minimax-m3":          {InPerM: 0.30, OutPerM: 1.20},
	"google/gemini-3.5-flash":     {InPerM: 1.50, OutPerM: 9.00},
	"moonshotai/kimi-k2-thinking": {InPerM: 0.60, OutPerM: 2.50},
	"deepseek/deepseek-v4-pro":    {InPerM: 0.435, OutPerM: 0.87, CacheReadPerM: 0.0036},
}

// CostOf returns the USD cost for a model's usage and whether the model was
// priced at all. ok=false means the slug is absent from Pricing — the caller
// renders "—", not 0, because free and unknown are different facts.
func CostOf(model string, u llm.Usage) (cost float64, ok bool) {
	// Try the ref verbatim (covers slash refs and bare slugs).
	if p, ok := Pricing[model]; ok {
		return p.Cost(u), true
	}

	// Try candidate-key resolution for colon-form refs.
	// Keep the list of five providers in sync with internal/cli.SplitModelRef.
	if i := strings.IndexByte(model, ':'); i > 0 {
		provider := model[:i]
		slug := model[i+1:]
		switch provider {
		case "openrouter", "anthropic", "xai", "openai", "ollama":
			// Try <provider>/<slug> (colon->slash normalization).
			if p, ok := Pricing[provider+"/"+slug]; ok {
				return p.Cost(u), true
			}
			// Try the bare <slug> (fallback).
			if p, ok := Pricing[slug]; ok {
				return p.Cost(u), true
			}
		}
	}

	return 0, false
}
