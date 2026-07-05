package llm

import (
	"encoding/json"
)

// FinishReason is the normalized reason generation stopped.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"           // natural end
	FinishLength        FinishReason = "length"         // hit a token cap
	FinishToolUse       FinishReason = "tool_use"       // model wants to call a tool
	FinishContentFilter FinishReason = "content_filter" // blocked by a safety filter
	FinishOther         FinishReason = "other"          // unrecognized / provider-specific
)

// Usage reports token accounting for a request. CachedTokens is the portion of
// PromptTokens served from a prompt cache, where the provider reports it.
// ReasoningTokens is the portion of CompletionTokens the model spent on hidden
// reasoning (thinking models: Gemini, o-series, DeepSeek). It is a SUBSET of
// CompletionTokens — already billed inside it — so never add it to a total; it
// is broken out only to see where completion spend goes.
//
// JSON tags are snake_case so every record that embeds Usage (agent transcripts,
// eval Trial files, council records, cmd/agent result JSON) uses one consistent
// shape on disk. Before 2026-07 the struct had no tags and the on-disk corpus
// (~600+ runs) used PascalCase keys ("PromptTokens"). The custom UnmarshalJSON
// accepts BOTH shapes so old unversioned records (eval Trial, council) don't
// silently zero out on read.
type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	Cost             float64 `json:"cost"` // native provider-reported cost in USD for this call; 0 when the provider does not report one.
}

// usageWire is the dual-shape helper for UnmarshalJSON: it declares both the
// current snake_case keys and the legacy PascalCase keys so a single
// json.Unmarshal can populate whichever shape the data carries. Snake_case wins
// when both are present (a field that is absent in the JSON leaves its pointer
// nil, which we treat as "not set").
type usageWire struct {
	PromptTokens           *int     `json:"prompt_tokens"`
	CompletionTokens       *int     `json:"completion_tokens"`
	TotalTokens            *int     `json:"total_tokens"`
	CachedTokens           *int     `json:"cached_tokens"`
	ReasoningTokens        *int     `json:"reasoning_tokens"`
	Cost                   *float64 `json:"cost"`
	LegacyPromptTokens     *int     `json:"PromptTokens"`
	LegacyCompletionTokens *int     `json:"CompletionTokens"`
	LegacyTotalTokens      *int     `json:"TotalTokens"`
	LegacyCachedTokens     *int     `json:"CachedTokens"`
	LegacyReasoningTokens  *int     `json:"ReasoningTokens"`
	LegacyCost             *float64 `json:"Cost"`
}

func (u *Usage) UnmarshalJSON(data []byte) error {
	var w usageWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	u.PromptTokens = pickInt(w.PromptTokens, w.LegacyPromptTokens)
	u.CompletionTokens = pickInt(w.CompletionTokens, w.LegacyCompletionTokens)
	u.TotalTokens = pickInt(w.TotalTokens, w.LegacyTotalTokens)
	u.CachedTokens = pickInt(w.CachedTokens, w.LegacyCachedTokens)
	u.ReasoningTokens = pickInt(w.ReasoningTokens, w.LegacyReasoningTokens)
	u.Cost = pickFloat(w.Cost, w.LegacyCost)
	return nil
}

// pickInt returns the snake_case value when present, otherwise the legacy value,
// otherwise 0.
func pickInt(snake, legacy *int) int {
	if snake != nil {
		return *snake
	}
	if legacy != nil {
		return *legacy
	}
	return 0
}

// pickFloat returns the snake_case value when present, otherwise the legacy
// value, otherwise 0.
func pickFloat(snake, legacy *float64) float64 {
	if snake != nil {
		return *snake
	}
	if legacy != nil {
		return *legacy
	}
	return 0
}

// Response is a provider-agnostic generation result. Raw holds the underlying
// SDK response for deep inspection of anything not normalized here (decision 8).
type Response struct {
	// Content is the generated content parts (text for now).
	Content []ContentPart

	// FinishReason is why generation stopped.
	FinishReason FinishReason

	// Usage is token accounting.
	Usage Usage

	// Model is the model that actually served the request, as reported by the
	// provider (may differ from the requested model, e.g. via OpenRouter).
	Model string

	// Raw is the underlying SDK response. Type-assert to the provider's
	// concrete type to reach un-normalized fields (logprobs, reasoning, etc.).
	Raw any
}

// Text returns the concatenated text of the response's content parts.
func (r *Response) Text() string { return partsText(r.Content) }
