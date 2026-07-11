package llm

// Request is a provider-agnostic generation request.
//
// Broadly-shared sampling params are typed fields (decision 6). Optional
// numeric knobs are pointers so "unset" is distinguishable from a deliberate
// zero. Provider-specific knobs ride in ProviderOptions and are interpreted by
// whichever adapter understands them; adapters ignore options they don't.
type Request struct {
	// Model overrides the provider's default model for this request.
	// Empty means "use the provider's configured default".
	Model string

	// System is a convenience top-level system prompt. Adapters place it
	// where the provider expects it (a system message for OpenAI-compatible
	// providers, a top-level field for Claude).
	System string

	// Messages is the conversation so far.
	Messages []Message

	// MaxTokens caps generated tokens. 0 means unset (provider default).
	MaxTokens int

	// Temperature, TopP, Seed are optional sampling controls.
	Temperature *float64
	TopP        *float64
	Seed        *int64

	// Stop is an optional set of stop sequences.
	Stop []string

	// Tools are the functions the model may call this turn. When non-empty, a
	// tool-capable adapter advertises them and may return ToolCallParts with
	// FinishToolUse; the caller executes them and replies with ToolResultParts.
	// Adapters that don't support tools ignore this (preserving swap-ability).
	Tools []Tool

	// ReasoningEffort asks a reasoning model to think harder or less
	// ("minimal" | "low" | "medium" | "high" | "xhigh" — passed through
	// untyped, so a new tier needs no code change). Empty means unset: the
	// provider's default, which is what every measurement before 2026-07-03
	// ran at. Adapters map it to their wire param (openaicompat →
	// `reasoning_effort`, the OpenAI chat-completions param OpenRouter
	// normalizes into its unified reasoning system); adapters without a
	// reasoning surface ignore it.
	ReasoningEffort string

	// ProviderOptions is a passthrough for provider-specific knobs (e.g.
	// reasoning effort, thinking budget). Adapters merge recognized keys into
	// the underlying request and ignore the rest. Prefer the typed helpers
	// each provider package exposes over setting raw keys here.
	ProviderOptions map[string]any
}
