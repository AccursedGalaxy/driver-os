// Package openaicompat is the adapter for every provider that speaks the
// OpenAI Chat Completions wire format: OpenAI itself, OpenRouter, X.AI (Grok),
// and local servers like Ollama / vLLM / LM Studio. They differ only by base
// URL, API key, and model id, so one adapter (parameterized by Config) plus a
// handful of preset constructors covers all of them.
package openaicompat

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

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
	return &Provider{
		name:   cfg.Name,
		model:  cfg.Model,
		client: openai.NewClient(opts...),
		// Streaming is wired in build step 2; tools/vision in later steps.
		caps: llm.Capabilities{Tools: true, Vision: true},
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
func Ollama(model string) *Provider {
	return New(Config{Name: "ollama", BaseURL: "http://localhost:11434/v1", APIKey: "ollama", Model: model})
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

// toMessages translates our message model to OpenAI message unions. For the
// slice, parts are flattened to text via Message.Text; tool/image parts are
// wired in later build steps.
func toMessages(req llm.Request) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		text := m.Text()
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, openai.SystemMessage(text))
		case llm.RoleAssistant:
			out = append(out, openai.AssistantMessage(text))
		default: // user, tool (until tools land), anything else
			out = append(out, openai.UserMessage(text))
		}
	}
	return out
}

func toResponse(cc *openai.ChatCompletion) *llm.Response {
	r := &llm.Response{Model: cc.Model, Raw: cc}
	if len(cc.Choices) > 0 {
		choice := cc.Choices[0]
		r.Content = []llm.ContentPart{llm.Text(choice.Message.Content)}
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
