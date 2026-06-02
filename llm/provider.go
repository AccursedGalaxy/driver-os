package llm

import (
	"context"
	"iter"
)

// Provider is the uniform interface every LLM backend implements. It is kept
// small (decision 2) so swapping providers is seamless for the common path.
//
// Capabilities that only some providers have (embeddings, ...) are exposed as
// separate optional interfaces promoted via type-assertion, not added here.
type Provider interface {
	// Name is the registered identity of this provider (e.g. "xai").
	Name() string

	// Capabilities advertises what this provider/model supports.
	Capabilities() Capabilities

	// Generate runs a single, non-streaming completion.
	Generate(ctx context.Context, req Request) (*Response, error)

	// Stream runs a completion incrementally, yielding Chunks as they arrive
	// (decision 5). The iterator yields content chunks (text deltas, completed
	// tool calls) followed by one terminal Done chunk carrying FinishReason and
	// Usage; a non-nil error ends the stream. A backend that cannot stream returns
	// UnsupportedStream and reports Streaming=false in Capabilities.
	Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error]
}

// Capabilities advertises optional features a provider supports, so callers
// can branch without triggering a failed request.
type Capabilities struct {
	Streaming  bool
	Tools      bool
	Vision     bool
	Embeddings bool
}
