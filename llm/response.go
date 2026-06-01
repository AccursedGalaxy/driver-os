package llm

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
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
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
