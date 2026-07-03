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
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"strings"
	"sync"

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

	// PromptCache, when true, marks Anthropic-style `cache_control` breakpoints on
	// the stable prefix (the system prompt) and the latest message of every
	// request, so a backend that supports prompt caching — Anthropic via OpenRouter
	// — serves the re-sent prefix from cache instead of re-billing it at full rate.
	// It is OPT-IN because the marker is an Anthropic extension: plain OpenAI
	// rejects an unknown field inside a content block, so this must stay off for
	// non-Anthropic targets. Harmless when the prefix is below the model's minimum
	// cacheable size (the backend simply doesn't cache). See WithPromptCache.
	PromptCache bool
}

// Provider is an OpenAI-compatible llm.Provider.
type Provider struct {
	name   string
	model  string
	client openai.Client
	caps   llm.Capabilities
	cache  bool // PromptCache: emit cache_control breakpoints (Anthropic-via-OpenRouter).

	// Provider pinning for OpenRouter Gemini encrypted-thought-signature
	// portability (see docs/backlog.md 2026-07-03). Gemini's
	// `reasoning_details` with format "google-gemini-v1" are encrypted
	// per-upstream: a signature minted by "Google AI Studio" fails on
	// "Google" (Vertex) with INVALID_ARGUMENT "Corrupted thought signature".
	// OpenRouter hops providers between turns under load, so the stickiness
	// must live in the adapter itself, not in the agent loop.
	//
	// Set by the first response that carries such a signature, then injected
	// into every subsequent request via OpenRouter's `provider` routing
	// field: `{"order":["<slug>"],"allow_fallbacks":false}`.
	mu             sync.Mutex
	pinnedProvider string // empty = not yet pinned
	isOpenRouter   bool   // gate: only engage for the OpenRouter constructor
}

// WithPromptCache returns the provider with Anthropic prompt-caching breakpoints
// enabled (see Config.PromptCache). Chainable onto a preset for the common case:
//
//	openaicompat.OpenRouter("anthropic/claude-opus-4.8").WithPromptCache()
//
// Use it ONLY for Anthropic-family models (the marker is their extension); leave
// it off for plain OpenAI, which rejects the field.
func (p *Provider) WithPromptCache() *Provider { p.cache = true; return p }

var _ llm.Provider = (*Provider)(nil)

// New builds a provider from an explicit Config. Use it for arbitrary or
// custom endpoints (a remote vLLM box, a non-standard local server); the
// preset constructors below cover the common providers.
func New(cfg Config) *Provider {
	opts := []option.RequestOption{
		option.WithBaseURL(cfg.BaseURL),
		// Strip SSE keep-alive comments before the SDK's decoder sees them: the
		// decoder turns "comment + blank line" into an EMPTY event and the JSON
		// parse of "" kills the stream — which is how every slow-to-first-token
		// (cheap/queued) model's stream died while fast ones never did. See
		// ssefilter.go.
		option.WithMiddleware(sseKeepaliveMiddleware),
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	// Default to the truth for a CLOUD OpenAI-compatible endpoint: native
	// tool-calling and streaming both work, vision does NOT yet (ImagePart isn't
	// wired through the adapters — see llm/content.go). Advertising vision here was
	// a flat lie; advertising tools for an arbitrary local model was optimism that
	// defeats the text-loop fallback — so callers that know better (the Ollama
	// preset, a custom vLLM box) override via Config.
	caps := llm.Capabilities{Tools: true, Streaming: true, Vision: false}
	if cfg.Capabilities != nil {
		caps = *cfg.Capabilities
	}

	// OpenRouter-specific: Gemini encrypted-thought-signature portability.
	// When OPENROUTER_PIN_PROVIDER is set (e.g. "google-vertex"), every
	// request pins to that upstream from turn 1; the sticky response-based
	// rule below then never needs to fire.
	isOR := cfg.Name == "openrouter"
	var pinned string
	if isOR {
		if slug := os.Getenv("OPENROUTER_PIN_PROVIDER"); slug != "" {
			pinned = slug
		}
	}

	return &Provider{
		name:           cfg.Name,
		model:          cfg.Model,
		client:         openai.NewClient(opts...),
		caps:           caps,
		cache:          cfg.PromptCache,
		isOpenRouter:   isOR,
		pinnedProvider: pinned,
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
		Capabilities: &llm.Capabilities{Tools: false, Streaming: true, Vision: false},
	})
}

func (p *Provider) Name() string                   { return p.name }
func (p *Provider) Capabilities() llm.Capabilities { return p.caps }

// Model is the requested model slug (e.g. "openai/gpt-5.5"). It is the caller's
// label for a run transcript; the actually-served model rides on Response.Model.
func (p *Provider) Model() string { return p.model }

// Generate runs a single non-streaming completion.
func (p *Provider) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	params, reqOpts := p.buildParams(req)
	cc, err := p.client.Chat.Completions.New(ctx, params, reqOpts...)
	if err != nil {
		return nil, p.classify(err)
	}

	// Sticky provider pinning: if the response carried Gemini encrypted
	// reasoning, record the responding upstream for subsequent turns.
	if p.isOpenRouter {
		providerRaw := json.RawMessage(cc.JSON.ExtraFields["provider"].Raw())
		var reasoningRaw json.RawMessage
		if len(cc.Choices) > 0 {
			reasoningRaw = json.RawMessage(cc.Choices[0].Message.JSON.ExtraFields["reasoning_details"].Raw())
		}
		p.maybePin(providerRaw, reasoningRaw)
	}

	return toResponse(cc), nil
}

// Stream runs a completion incrementally (decision 5). It yields text deltas as
// they arrive, then the turn's reasoning trace once fully reassembled (a
// Reasoning chunk — `reasoning_details` streams as block fragments, and only the
// complete trace is replayable; see reasoningAccumulator), then each tool call
// once fully assembled, then one terminal Done chunk carrying FinishReason +
// Usage. The same request mapping as Generate feeds it (buildParams), so caching,
// tools, and sampling knobs behave identically; only the transport differs.
// Errors mid-stream are classified like Generate's and end the iteration.
func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		params, reqOpts := p.buildParams(req)
		// Ask for the trailing usage chunk; without this the stream reports no
		// token accounting and the terminal Done chunk's Usage stays zero.
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}

		stream := p.client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
		defer stream.Close()

		// The accumulator reassembles partial deltas (text and tool-call arg
		// fragments) into a complete ChatCompletion. We DON'T take usage from it:
		// AddChunk sums the top-level token counts but drops PromptTokensDetails, so
		// acc.Usage loses CachedTokens. We capture the raw usage-bearing chunk
		// instead, keeping streaming token accounting identical to Generate's.
		// (include_usage sends usage only on the final chunk; "last chunk that
		// carries usage wins" is robust regardless.)
		acc := openai.ChatCompletionAccumulator{}
		var reasoning reasoningAccumulator
		var usage openai.CompletionUsage
		var providerRaw json.RawMessage // OpenRouter: captured for sticky pinning.
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			if chunk.Usage.TotalTokens > 0 {
				usage = chunk.Usage
			}

			// Capture the responding upstream from the first chunk that carries it
			// (every SSE chunk carries a top-level `provider` field on OpenRouter).
			if p.isOpenRouter && len(providerRaw) == 0 {
				if raw := chunk.JSON.ExtraFields["provider"].Raw(); raw != "" && raw != "null" {
					providerRaw = json.RawMessage(raw)
				}
			}

			if len(chunk.Choices) > 0 {
				// Reassemble the reasoning trace: the SDK accumulator drops
				// unmodeled extra fields, so `reasoning_details` fragments must be
				// merged here or the streamed turn loses its trace (fatal for
				// Gemini's encrypted signature — see llm.ReasoningPart). Same
				// Raw()-not-Valid() gate as toResponse.
				if raw := chunk.Choices[0].Delta.JSON.ExtraFields["reasoning_details"].Raw(); raw != "" && raw != "null" {
					reasoning.add(json.RawMessage(raw))
				}
				// Stream text deltas live — that's the value of streaming. Skip empty
				// deltas (usage-only and role-only chunks carry no text).
				if d := chunk.Choices[0].Delta.Content; d != "" {
					if !yield(llm.Chunk{Text: d}, nil) {
						return
					}
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(llm.Chunk{}, p.classify(err))
			return
		}

		// Sticky provider pinning: if the stream carried Gemini encrypted
		// reasoning, record the responding upstream for subsequent turns.
		if p.isOpenRouter {
			if raw, _ := reasoning.raw(); raw != nil {
				p.maybePin(providerRaw, raw)
			}
		}

		// The reassembled reasoning trace, surfaced like a tool call: once,
		// complete, at end-of-stream (a partial trace is not replayable — a
		// truncated signature is exactly the corruption replay must avoid). ONE
		// chunk carrying the whole array, mirroring the single ReasoningPart the
		// non-streaming path attaches.
		if raw, ok := reasoning.raw(); ok {
			if !yield(llm.Chunk{Reasoning: &llm.ReasoningPart{Raw: raw}}, nil) {
				return
			}
		}

		// Tool calls are surfaced from the FULLY accumulated result, not live: the
		// SDK's JustFinishedToolCall is documented as unreliable when a turn has
		// PARALLEL tool calls (multiple calls, arg fragments interleaved by index),
		// dropping calls — whereas the accumulator assembles every call correctly
		// regardless of interleaving. A tool call is unusable until its args are
		// complete anyway, so emitting them at end-of-stream (after the live text,
		// before Done) costs the caller nothing and is correct for N calls.
		if len(acc.Choices) > 0 {
			for _, tc := range acc.Choices[0].Message.ToolCalls {
				part := &llm.ToolCallPart{ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments)}
				if !yield(llm.Chunk{ToolCall: part}, nil) {
					return
				}
			}
		}

		// Terminal chunk: the run's finish reason and full token accounting.
		final := llm.Chunk{Done: true, Usage: usageFrom(usage)}
		if len(acc.Choices) > 0 {
			final.FinishReason = mapFinish(acc.Choices[0].FinishReason)
		}
		yield(final, nil)
	}
}

// buildParams maps a provider-agnostic Request onto the SDK's params plus the
// per-request options, shared verbatim by Generate and Stream so the two paths
// can never drift on model selection, cache breakpoints, sampling, or tools.
func (p *Provider) buildParams(req llm.Request) (openai.ChatCompletionNewParams, []option.RequestOption) {
	model := req.Model
	if model == "" {
		model = p.model
	}

	params := openai.ChatCompletionNewParams{
		Model:    model,
		Messages: toMessages(req),
	}
	if p.cache {
		// (HP-8) Mark cache breakpoints so the re-sent prefix is served from the
		// provider's prompt cache instead of re-billed every turn.
		applyCacheBreakpoints(params.Messages)
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
	if req.ReasoningEffort != "" {
		// The OpenAI chat-completions param; OpenRouter accepts it too and
		// folds it into its unified `reasoning` config for other upstreams.
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
	}

	// Provider pinning for OpenRouter Gemini encrypted-signature portability.
	p.mu.Lock()
	pinned := p.pinnedProvider
	p.mu.Unlock()
	if pinned != "" {
		params.SetExtraFields(map[string]any{
			"provider": map[string]any{
				"order":           []string{pinned},
				"allow_fallbacks": false,
			},
		})
	}

	// Provider-specific passthrough (decision 6): merge recognized-by-the-wire
	// keys into the request body. Unknown keys are simply sent; the server
	// ignores what it doesn't use.
	reqOpts := make([]option.RequestOption, 0, len(req.ProviderOptions))
	for k, v := range req.ProviderOptions {
		reqOpts = append(reqOpts, option.WithJSONSet(k, v))
	}
	return params, reqOpts
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

// assistantMessage builds an assistant message, attaching any tool-call parts and
// re-attaching the turn's reasoning trace. With no tool calls and no reasoning it
// is a plain text assistant message (the common case).
func assistantMessage(m llm.Message) openai.ChatCompletionMessageParamUnion {
	var calls []openai.ChatCompletionMessageToolCallParam
	var reasoning json.RawMessage
	for _, p := range m.Parts {
		switch tp := p.(type) {
		case llm.ToolCallPart:
			// The wire field is a JSON STRING and must be valid JSON. A tool call
			// with no arguments has empty Args; serialize that as "{}" (an empty
			// object), never "" — an empty string is not valid JSON and a strict
			// server rejects the replayed assistant turn.
			args := string(tp.Args)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, openai.ChatCompletionMessageToolCallParam{
				ID: tp.ID,
				Function: openai.ChatCompletionMessageToolCallFunctionParam{
					Name:      tp.Name,
					Arguments: args,
				},
			})
		case llm.ReasoningPart:
			reasoning = tp.Raw
		}
	}
	if len(calls) == 0 && len(reasoning) == 0 {
		return openai.AssistantMessage(m.Text())
	}
	am := openai.ChatCompletionAssistantMessageParam{}
	if len(calls) > 0 {
		am.ToolCalls = calls
	}
	if txt := m.Text(); txt != "" { // preserve any narration emitted alongside the calls.
		am.Content.OfString = openai.String(txt)
	}
	// Replay the model's reasoning trace as the `reasoning_details` extra field
	// (it's an OpenRouter extension absent from the OpenAI types). Decoded to a
	// generic value so the SDK's encoder re-emits valid JSON regardless of how the
	// provider ordered the object. This is what keeps Gemini's encrypted thought
	// signature alive across tool calls — without it the model spirals (P5 kill).
	if len(reasoning) > 0 {
		var v any
		if json.Unmarshal(reasoning, &v) == nil {
			am.SetExtraFields(map[string]any{"reasoning_details": v})
		}
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &am}
}

// applyCacheBreakpoints marks the two cache anchors that maximize reuse of our
// re-send-everything loop (P1) under Anthropic prompt caching (via OpenRouter):
//
//   - the SYSTEM message (the prefix that is byte-identical on every turn — the
//     system prompt + the tool schemas that precede it in Anthropic's cache order);
//   - the LAST message of the request (this turn's newest content). Anthropic
//     reads the longest cached prefix automatically, so marking the tail each turn
//     WRITES an ever-growing cache that the NEXT turn READS in full — incremental
//     caching that collapses the quadratic re-send cost (HP-8).
//
// Two breakpoints (Anthropic allows up to four) is enough: system for the stable
// base, tail for the growing history. Below the model's minimum cacheable size
// the backend just ignores the markers, so this is always safe to apply.
func applyCacheBreakpoints(msgs []openai.ChatCompletionMessageParamUnion) {
	if len(msgs) == 0 {
		return
	}
	if msgs[0].OfSystem != nil { // the stable system+tools prefix.
		markCacheBreakpoint(&msgs[0])
	}
	markCacheBreakpoint(&msgs[len(msgs)-1]) // the growing-history tail.
}

// ephemeralCacheControl is the marker OpenRouter forwards to Anthropic to open a
// cache breakpoint on a content block.
func ephemeralCacheControl() map[string]any {
	return map[string]any{"cache_control": map[string]any{"type": "ephemeral"}}
}

// markCacheBreakpoint rewrites a message's string content into the single-element
// ARRAY form carrying a cache_control marker on its text block — the only shape
// that can hold the marker (it's a per-content-block field, not a message field).
// It handles the kinds that actually anchor a breakpoint in our loops: system, the
// trailing user OBSERVATION (text loop), and the trailing tool result (native
// loop). Other kinds are left untouched. The marker rides as an SDK "extra field",
// since cache_control is an Anthropic extension absent from the OpenAI types.
func markCacheBreakpoint(msg *openai.ChatCompletionMessageParamUnion) {
	textPart := func(s string) openai.ChatCompletionContentPartTextParam {
		p := openai.ChatCompletionContentPartTextParam{Text: s}
		p.SetExtraFields(ephemeralCacheControl())
		return p
	}
	switch {
	case msg.OfSystem != nil:
		msg.OfSystem.Content = openai.ChatCompletionSystemMessageParamContentUnion{
			OfArrayOfContentParts: []openai.ChatCompletionContentPartTextParam{
				textPart(msg.OfSystem.Content.OfString.Or("")),
			},
		}
	case msg.OfTool != nil:
		msg.OfTool.Content = openai.ChatCompletionToolMessageParamContentUnion{
			OfArrayOfContentParts: []openai.ChatCompletionContentPartTextParam{
				textPart(msg.OfTool.Content.OfString.Or("")),
			},
		}
	case msg.OfUser != nil:
		part := textPart(msg.OfUser.Content.OfString.Or(""))
		msg.OfUser.Content = openai.ChatCompletionUserMessageParamContentUnion{
			OfArrayOfContentParts: []openai.ChatCompletionContentPartUnionParam{{OfText: &part}},
		}
	}
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
		// Preserve any provider reasoning trace (OpenRouter `reasoning_details`, an
		// untyped extra field) so the loop can replay it on this assistant turn.
		// Gemini's is an ENCRYPTED thought signature it MUST see again to continue
		// across tool calls — dropping it makes the model spiral (see llm.ReasoningPart).
		// Captured raw and never interpreted.
		// NOTE: gate on Raw(), not Valid() — the SDK stores extra (unmodeled) fields
		// with a status that reports Valid()==false even when the raw JSON is present.
		if raw := choice.Message.JSON.ExtraFields["reasoning_details"].Raw(); raw != "" && raw != "null" {
			r.Content = append(r.Content, llm.ReasoningPart{Raw: json.RawMessage(raw)})
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
	r.Usage = usageFrom(cc.Usage)
	return r
}

// usageFrom normalizes the SDK's token accounting into our Usage. Shared by the
// non-streaming response and the streaming Done chunk so both report tokens the
// same way (CachedTokens included, where the backend populates it).
func usageFrom(u openai.CompletionUsage) llm.Usage {
	return llm.Usage{
		PromptTokens:     int(u.PromptTokens),
		CompletionTokens: int(u.CompletionTokens),
		TotalTokens:      int(u.TotalTokens),
		CachedTokens:     int(u.PromptTokensDetails.CachedTokens),
		ReasoningTokens:  int(u.CompletionTokensDetails.ReasoningTokens),
	}
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

	// Mid-stream failures never reach the *openai.Error branch below: the
	// SDK's ssestream surfaces an SSE error EVENT as a plain
	// fmt.Errorf("received error while streaming: %s", <error JSON>) and a
	// dropped connection as the raw transport error. Both mean "this attempt
	// died after the request was accepted" — e.g. OpenRouter's
	// {"code":504,"message":"Upstream idle timeout exceeded"} when a slow
	// upstream goes quiet — which is transient, not a fault in the request.
	// Left unclassified they read as Retryable:false and killed whole runs
	// (run 20260702-204848). Marking them retryable lets the agent's
	// generateWithRetry re-send; a streamed error never commits the turn, so
	// the re-send is safe.
	if payload, ok := streamErrPayload(err); ok {
		var e struct {
			Code    json.Number `json:"code"`
			Message string      `json:"message"`
		}
		_ = json.Unmarshal([]byte(payload), &e)
		code, _ := e.Code.Int64()
		kind, retryable := llm.KindUnknown, true
		switch {
		case isContextLength(e.Message):
			// A window overflow can arrive mid-stream too; route it to the
			// eviction fallback, never to a verbatim (still overlong) retry.
			kind, retryable = llm.KindContextLength, false
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			kind, retryable = llm.KindAuth, false
		case code == http.StatusTooManyRequests:
			kind = llm.KindRateLimit
		}
		return &llm.ProviderError{Provider: p.name, Kind: kind, StatusCode: int(code), Retryable: retryable, Err: err}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) { // connection dropped mid-body
		return &llm.ProviderError{Provider: p.name, Kind: llm.KindUnknown, Retryable: true, Err: err}
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

// streamErrPayload extracts the provider error JSON from the SDK's mid-stream
// error ("received error while streaming: {...}"). The prefix is the stable
// contract of openai-go's ssestream (packages/ssestream, pinned v1.12.0).
func streamErrPayload(err error) (string, bool) {
	_, payload, ok := strings.Cut(err.Error(), "received error while streaming: ")
	return payload, ok
}

func isContextLength(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context length") ||
		strings.Contains(m, "context_length") ||
		strings.Contains(m, "maximum context") ||
		strings.Contains(m, "too many tokens") ||
		strings.Contains(m, "reduce the length")
}

// pinProviderName maps OpenRouter's display name for an upstream to the routing
// slug expected by the `provider.order` request field. An unknown name returns
// "" (fail open — do not pin).
func pinProviderName(display string) string {
	switch display {
	case "Google AI Studio":
		return "google-ai-studio"
	case "Google":
		return "google-vertex"
	}
	return ""
}

// hasEncryptedReasoning returns true when raw JSON contains any
// `reasoning_details` entry whose "format" is "google-gemini-v1" (Gemini's
// per-upstream encrypted thought signature). It parses minimally — only enough
// to decide stickiness — and never interprets the payload.
func hasEncryptedReasoning(raw json.RawMessage) bool {
	var entries []struct {
		Format string `json:"format"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return false
	}
	for _, e := range entries {
		if e.Format == "google-gemini-v1" {
			return true
		}
	}
	return false
}

// maybePin is the response-side of the sticky-provider rule. After a
// (successful) Generate or Stream turn that carried encrypted reasoning,
// record the responding upstream so every subsequent request uses the same one.
// Does nothing for non-OpenRouter providers, when already pinned, or when the
// provider name can't be mapped.
func (p *Provider) maybePin(providerField json.RawMessage, reasoningRaw json.RawMessage) {
	if !p.isOpenRouter {
		return
	}
	if !hasEncryptedReasoning(reasoningRaw) {
		return
	}

	var name string
	if json.Unmarshal(providerField, &name) != nil || name == "" {
		return
	}
	slug := pinProviderName(name)
	if slug == "" {
		if os.Getenv("DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "openaicompat: OpenRouter responded via unknown provider %q, not pinning\n", name)
		}
		return
	}

	p.mu.Lock()
	if p.pinnedProvider == "" {
		p.pinnedProvider = slug
	}
	p.mu.Unlock()
}
