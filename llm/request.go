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

	// ProviderOptions is a passthrough for provider-specific knobs (e.g.
	// reasoning effort, thinking budget). Adapters merge recognized keys into
	// the underlying request and ignore the rest. Prefer the typed helpers
	// each provider package exposes over setting raw keys here.
	ProviderOptions map[string]any
}
