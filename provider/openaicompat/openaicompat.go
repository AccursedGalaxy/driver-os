// Package openaicompat is the adapter for every provider that speaks the
// OpenAI Chat Completions wire format: OpenAI itself, OpenRouter, X.AI (Grok),
// and local servers like Ollama / vLLM / LM Studio. They differ only by base
// URL, API key, and model id, so one adapter (parameterized by Config) plus a
// handful of preset constructors covers all of them.
package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Config configures an OpenAI-compatible provider.
type Config struct {
	// Name is the provider's identity (e.g. "xai"). Used in errors and the registry.
	Name string
	// BaseURL is the API base, e.g. "https://api.x.ai/v1".
	BaseURL string
	// APIKey authenticates requests. May be empty for keyless local servers.
	APIKey string
	// Model is the default model id used when a Request omits Model.
	Model string
	// HTTPClient optionally overrides the HTTP client (timeouts, proxies, ...).
	HTTPClient *http.Client
	// Capabilities optionally overrides what this provider/model advertises
	// (native tool-calling, vision, ...). nil = the constructor's default. It is
	// per-CONFIG, not a family constant, because tool support is really a model
	// property: a cloud endpoint's models all support it, an arbitrary local
	// model (Ollama, a custom vLLM box) may not — and the loop selector
	// (cmd/agent) trusts this to decide native-tools vs the text fallback, so an
	// optimistic lie here means no fallback and a failed request.
	Capabilities *llm.Capabilities
}

// Provider is an OpenAI-compatible llm.Provider.
type Provider struct {
	name   string
	model  string
	client openai.Client
	caps   llm.Capabilities
}

var _ llm.Provider = (*Provider)(nil)

// New builds a provider from an explicit Config. Use it for arbitrary or
// custom endpoints (a remote vLLM box, a non-standard local server); the
// preset constructors below cover the common providers.
func New(cfg Config) *Provider {
	opts := []option.RequestOption{option.WithBaseURL(cfg.BaseURL)}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	// Default to the truth for a CLOUD OpenAI-compatible endpoint: native
	// tool-calling works, vision does NOT yet (ImagePart isn't wired through the
	// adapters — see llm/content.go), and streaming arrives in a later build step.
	// Advertising vision here was a flat lie; advertising tools for an arbitrary
	// local model was optimism that defeats the text-loop fallback — so callers
	// that know better (the Ollama preset, a custom vLLM box) override via Config.
	caps := llm.Capabilities{Tools: true, Vision: false}
	if cfg.Capabilities != nil {
		caps = *cfg.Capabilities
	}
	return &Provider{
		name:   cfg.Name,
		model:  cfg.Model,
		client: openai.NewClient(opts...),
		caps:   caps,
	}
}

// Preset constructors read the conventional env var for their key and default
// the base URL. They take just the model id, keeping registration terse.

// OpenAI targets the OpenAI API (OPENAI_API_KEY).
func OpenAI(model string) *Provider {
	return New(Config{Name: "openai", BaseURL: "https://api.openai.com/v1", APIKey: os.Getenv("OPENAI_API_KEY"), Model: model})
}

// XAI targets the X.AI (Grok) API (X_AI_API_KEY).
func XAI(model string) *Provider {
	return New(Config{Name: "xai", BaseURL: "https://api.x.ai/v1", APIKey: os.Getenv("X_AI_API_KEY"), Model: model})
}

// OpenRouter targets the OpenRouter API (OPENROUTER_API_KEY). Model ids are
// namespaced, e.g. "openai/gpt-4o-mini" or "x-ai/grok-4-fast".
func OpenRouter(model string) *Provider {
	return New(Config{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: os.Getenv("OPENROUTER_API_KEY"), Model: model})
}

// Ollama targets a local Ollama server. The key is a placeholder Ollama ignores.
// Tool support is per-MODEL with Ollama (many small models can't function-call),
// so it defaults to no native tools — the agent then uses the transparent text
// loop, which works everywhere. A tool-capable local model opts in by building
// the provider with New(Config{..., Capabilities: &llm.Capabilities{Tools: true}}).
func Ollama(model string) *Provider {
	return New(Config{
		Name: "ollama", BaseURL: "http://localhost:11434/v1", APIKey: "ollama", Model: model,
		Capabilities: &llm.Capabilities{Tools: false, Vision: false},
	})
}

func (p *Provider) Name() string                   { return p.name }
func (p *Provider) Capabilities() llm.Capabilities { return p.caps }

// Generate runs a single non-streaming completion.
func (p *Provider) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	model := req.Model
	if model == "" {
		model = p.model
	}

	params := openai.ChatCompletionNewParams{
		Model:    model,
		Messages: toMessages(req),
	}
	if req.MaxTokens > 0 {
		params.MaxTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Temperature != nil {
		params.Temperature = openai.Float(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = openai.Float(*req.TopP)
	}
	if req.Seed != nil {
		params.Seed = openai.Int(*req.Seed)
	}
	if len(req.Stop) > 0 {
		params.Stop = openai.ChatCompletionNewParamsStopUnion{OfStringArray: req.Stop}
	}
	if tools := toTools(req.Tools); len(tools) > 0 {
		params.Tools = tools
	}

	// Provider-specific passthrough (decision 6): merge recognized-by-the-wire
	// keys into the request body. Unknown keys are simply sent; the server
	// ignores what it doesn't use.
	reqOpts := make([]option.RequestOption, 0, len(req.ProviderOptions))
	for k, v := range req.ProviderOptions {
		reqOpts = append(reqOpts, option.WithJSONSet(k, v))
	}

	cc, err := p.client.Chat.Completions.New(ctx, params, reqOpts...)
	if err != nil {
		return nil, p.classify(err)
	}
	return toResponse(cc), nil
}

// toMessages translates our message model to OpenAI message unions. Text parts
// flatten via Message.Text; an assistant turn additionally carries any tool-call
// parts, and a RoleTool message becomes one `tool` message per result — so a
// tool-calling round-trip (assistant tool_calls -> tool results) replays exactly
// as the API requires (image parts remain a later build step).
func toMessages(req llm.Request) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, openai.SystemMessage(m.Text()))
		case llm.RoleAssistant:
			out = append(out, assistantMessage(m))
		case llm.RoleTool:
			// One `tool` message per result part — each answers a specific call id.
			// NOTE: Chat Completions has no per-tool-message error flag (unlike
			// Anthropic's is_error), so ToolResultPart.IsError is NOT representable
			// on the wire here — the failure is conveyed to the model by the result
			// CONTENT itself (the loop prefixes failures with "ERROR:"). The IsError
			// bit stays meaningful for providers/adapters that can carry it.
			for _, p := range m.Parts {
				if tr, ok := p.(llm.ToolResultPart); ok {
					out = append(out, openai.ToolMessage(tr.Content, tr.ToolCallID))
				}
			}
		default: // user, anything else
			out = append(out, openai.UserMessage(m.Text()))
		}
	}
	return out
}

// assistantMessage builds an assistant message, attaching any tool-call parts.
// With no tool calls it is a plain text assistant message (the common case).
func assistantMessage(m llm.Message) openai.ChatCompletionMessageParamUnion {
	var calls []openai.ChatCompletionMessageToolCallParam
	for _, p := range m.Parts {
		if tc, ok := p.(llm.ToolCallPart); ok {
			calls = append(calls, openai.ChatCompletionMessageToolCallParam{
				ID: tc.ID,
				Function: openai.ChatCompletionMessageToolCallFunctionParam{
					Name:      tc.Name,
					Arguments: string(tc.Args),
				},
			})
		}
	}
	if len(calls) == 0 {
		return openai.AssistantMessage(m.Text())
	}
	am := openai.ChatCompletionAssistantMessageParam{ToolCalls: calls}
	if txt := m.Text(); txt != "" { // preserve any narration emitted alongside the calls.
		am.Content.OfString = openai.String(txt)
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &am}
}

// toTools translates provider-agnostic tools into OpenAI tool params. Schema (a
// JSON Schema object) unmarshals into the SDK's parameter map; an empty/invalid
// schema yields an empty parameter list, which the API accepts as a no-arg tool.
func toTools(tools []llm.Tool) []openai.ChatCompletionToolParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		fn := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			fn.Description = openai.String(t.Description)
		}
		if len(t.Schema) > 0 {
			var params shared.FunctionParameters
			if err := json.Unmarshal(t.Schema, &params); err == nil {
				fn.Parameters = params
			}
		}
		out = append(out, openai.ChatCompletionToolParam{Function: fn})
	}
	return out
}

func toResponse(cc *openai.ChatCompletion) *llm.Response {
	r := &llm.Response{Model: cc.Model, Raw: cc}
	if len(cc.Choices) > 0 {
		choice := cc.Choices[0]
		if choice.Message.Content != "" {
			r.Content = append(r.Content, llm.Text(choice.Message.Content))
		}
		for _, tc := range choice.Message.ToolCalls {
			r.Content = append(r.Content, llm.ToolCallPart{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: json.RawMessage(tc.Function.Arguments),
			})
		}
		r.FinishReason = mapFinish(choice.FinishReason)
	}
	r.Usage = llm.Usage{
		PromptTokens:     int(cc.Usage.PromptTokens),
		CompletionTokens: int(cc.Usage.CompletionTokens),
		TotalTokens:      int(cc.Usage.TotalTokens),
		CachedTokens:     int(cc.Usage.PromptTokensDetails.CachedTokens),
	}
	return r
}

func mapFinish(reason string) llm.FinishReason {
	switch reason {
	case "stop":
		return llm.FinishStop
	case "length":
		return llm.FinishLength
	case "tool_calls", "function_call":
		return llm.FinishToolUse
	case "content_filter":
		return llm.FinishContentFilter
	default:
		return llm.FinishOther
	}
}

// classify maps SDK/transport errors to a normalized llm.ProviderError.
func (p *Provider) classify(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &llm.ProviderError{Provider: p.name, Kind: llm.KindCanceled, Err: err}
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		kind := llm.KindUnknown
		retryable := false
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = llm.KindAuth
		case http.StatusTooManyRequests:
			kind = llm.KindRateLimit
			retryable = true
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			if isContextLength(apiErr.Message) {
				kind = llm.KindContextLength
			}
		case http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			retryable = true
		}
		return &llm.ProviderError{
			Provider:   p.name,
			Kind:       kind,
			StatusCode: apiErr.StatusCode,
			Retryable:  retryable,
			Err:        err,
		}
	}

	return &llm.ProviderError{Provider: p.name, Kind: llm.KindUnknown, Err: err}
}

func isContextLength(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context length") ||
		strings.Contains(m, "context_length") ||
		strings.Contains(m, "maximum context") ||
		strings.Contains(m, "too many tokens") ||
		strings.Contains(m, "reduce the length")
}
