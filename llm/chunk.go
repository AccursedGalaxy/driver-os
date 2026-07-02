package llm

import (
	"errors"
	"iter"
)

// Chunk is one increment of a streaming generation (decision 5: the core
// streaming primitive is iter.Seq2[Chunk, error]). A stream yields a sequence of
// content chunks followed by exactly one terminal chunk:
//
//   - Text holds an incremental text delta. It is the common chunk; concatenating
//     every Text across a stream reconstructs the full message text.
//   - ToolCall is a tool call surfaced ONCE FULLY ASSEMBLED (id + name + complete
//     JSON args), not as raw argument fragments — a streamed tool call arrives as
//     partial JSON across many wire chunks, and almost every caller wants the
//     whole call, so the adapter accumulates it and emits it complete. nil on a
//     text chunk.
//   - Reasoning is a provider reasoning trace surfaced ONCE FULLY ASSEMBLED,
//     like ToolCall — a stateful trace (Anthropic's signed thinking blocks) is
//     only replayable complete, and replay is the sole reason it exists (see
//     ReasoningPart). A provider that returns none never yields this shape.
//   - Done marks the single TERMINAL chunk. It carries no content; instead it
//     reports FinishReason and the final Usage for the whole generation. It is the
//     streaming analogue of a Response's tail fields, so a caller that only needs
//     totals can ignore every prior chunk and read this one.
//
// A chunk is one of those four shapes — a text delta, a completed tool call, an
// assembled reasoning trace, or the terminal Done — never a mix.
type Chunk struct {
	// Text is an incremental text delta (empty on non-text chunks).
	Text string

	// ToolCall is a fully-assembled tool call (nil unless this chunk is a call).
	ToolCall *ToolCallPart

	// Reasoning is a fully-assembled reasoning trace for the turn (nil unless
	// this chunk carries one). The caller must keep it on the assistant turn it
	// re-sends, exactly like a ReasoningPart from Generate.
	Reasoning *ReasoningPart

	// FinishReason is why generation stopped. Set only on the Done chunk.
	FinishReason FinishReason

	// Usage is token accounting for the whole generation. Set only on the Done chunk.
	Usage Usage

	// Done is true on the single terminal chunk that carries FinishReason+Usage.
	Done bool
}

// UnsupportedStream is the Stream implementation for providers that do not stream:
// it yields a single KindUnsupported error and nothing else. Streaming is part of
// the core Provider interface, but a backend (or a test fake) that only implements
// Generate can satisfy the method with this instead of hand-writing the iterator.
func UnsupportedStream(provider string) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		yield(Chunk{}, &ProviderError{
			Provider: provider,
			Kind:     KindUnsupported,
			Err:      errors.New("streaming not supported by this provider"),
		})
	}
}
