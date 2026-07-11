// Package anthropic is the native adapter for Anthropic's Messages API
// (Claude), via the official anthropic-sdk-go. It exists alongside the
// OpenRouter route (openaicompat with "anthropic/..." models) for the things
// only the first-party API gives us: exact cache_control placement billed at
// Anthropic's cache rates with no middleman fee, thinking blocks whose signed
// signatures round-trip without an OpenRouter translation layer in between
// (the layer that has burned us twice — see llm.ReasoningPart), and the
// native `output_config.effort` knob.
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// DefaultMaxTokens is used when a Request leaves MaxTokens unset. Unlike the
// OpenAI wire format, the Messages API REQUIRES max_tokens on every request
// (and treats 0 as "warm the cache, generate nothing"), so the adapter must
// always send a real cap.
const DefaultMaxTokens = 8192

// Config configures the Anthropic provider.
type Config struct {
	// Name is the provider's identity in errors and the registry. Defaults to
	// "anthropic".
	Name string
	// APIKey authenticates requests (ANTHROPIC_API_KEY). Note this must be an
	// API key with credits — Claude subscription (Pro/Max) OAuth tokens are
	// restricted to Anthropic's own surfaces and do not work here.
	APIKey string
	// BaseURL optionally overrides the API base (tests, proxies). Empty means
	// the SDK default (https://api.anthropic.com).
	BaseURL string
	// Model is the default model id used when a Request omits Model.
	Model string
	// MaxTokens is the max_tokens sent when a Request leaves it unset. 0 means
	// DefaultMaxTokens (the field exists because the wire REQUIRES a value).
	MaxTokens int
	// HTTPClient optionally overrides the HTTP client (timeouts, proxies, ...).
	HTTPClient *http.Client
	// Capabilities optionally overrides what this provider advertises.
	// nil = the default (tools + streaming + vision — ImagePart is now wired
	// through the adapter).
	Capabilities *llm.Capabilities
	// PromptCache marks cache_control breakpoints (system prefix + request
	// tail) so the re-sent prefix of the loop is served from Anthropic's
	// prompt cache instead of re-billed at full rate every turn (HP-8, same
	// two anchors as openaicompat's breakpoints). Unlike openaicompat this is
	// safe to default ON — every model behind this adapter is Anthropic's —
	// so the Claude preset enables it.
	PromptCache bool
}

// Provider is the native Anthropic llm.Provider.
type Provider struct {
	name      string
	model     string
	maxTokens int
	client    sdk.Client
	caps      llm.Capabilities
	cache     bool
}

var _ llm.Provider = (*Provider)(nil)

// New builds a provider from an explicit Config.
func New(cfg Config) *Provider {
	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	caps := llm.Capabilities{Tools: true, Streaming: true, Vision: true}
	if cfg.Capabilities != nil {
		caps = *cfg.Capabilities
	}
	name := cfg.Name
	if name == "" {
		name = "anthropic"
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	return &Provider{
		name:      name,
		model:     cfg.Model,
		maxTokens: maxTokens,
		client:    sdk.NewClient(opts...),
		caps:      caps,
		cache:     cfg.PromptCache,
	}
}

// Claude is the preset constructor: the Anthropic API with the key from the
// env (ANTHROPIC_API_KEY, or CLAUDE_API_KEY as an alias) and prompt caching on
// (pure win on first-party billing).
func Claude(model string) *Provider {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		key = os.Getenv("CLAUDE_API_KEY")
	}
	return New(Config{
		Name:        "anthropic",
		APIKey:      key,
		Model:       model,
		PromptCache: true,
	})
}

func (p *Provider) Name() string                   { return p.name }
func (p *Provider) Capabilities() llm.Capabilities { return p.caps }

// Model is the requested model slug (e.g. "claude-sonnet-5"). It is the
// caller's label for a run transcript; the actually-served model rides on
// Response.Model.
func (p *Provider) Model() string { return p.model }

// Generate runs a single non-streaming completion.
func (p *Provider) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	params, reqOpts := p.buildParams(req)
	msg, err := p.client.Messages.New(ctx, params, reqOpts...)
	if err != nil {
		return nil, p.classify(err)
	}
	return toResponse(msg), nil
}

// Stream runs a completion incrementally (decision 5). Text deltas stream
// live; each tool call is surfaced once fully assembled (the SDK accumulator
// joins the input_json_delta fragments), as is each thinking block (a
// Reasoning chunk — the signature only exists complete, and the API rejects a
// replayed tool-using turn missing its signed thinking, so the caller MUST get
// these to re-send). Then one terminal Done chunk carries FinishReason +
// Usage. Thinking deltas are never streamed as text — they'd pollute the prose
// transcript.
func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		params, reqOpts := p.buildParams(req)
		stream := p.client.Messages.NewStreaming(ctx, params, reqOpts...)
		defer stream.Close()

		acc := sdk.Message{}
		for stream.Next() {
			event := stream.Current()
			if err := acc.Accumulate(event); err != nil {
				yield(llm.Chunk{}, p.classify(err))
				return
			}
			// Live text only: content_block_delta / text_delta. Thinking and
			// input_json deltas are accumulated, not streamed.
			if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				if !yield(llm.Chunk{Text: event.Delta.Text}, nil) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(llm.Chunk{}, p.classify(err))
			return
		}

		// Thinking blocks and tool calls from the fully-accumulated message
		// (complete blocks only — a partial call is unusable, and a thinking
		// block's signature arrives last). Thinking first, mirroring the API's
		// own block order.
		for _, block := range acc.Content {
			switch block.Type {
			case "thinking", "redacted_thinking":
				part := &llm.ReasoningPart{Raw: json.RawMessage(block.RawJSON())}
				if !yield(llm.Chunk{Reasoning: part}, nil) {
					return
				}
			case "tool_use":
				part := &llm.ToolCallPart{ID: block.ID, Name: block.Name, Args: rawOrEmptyObject(block.Input)}
				if !yield(llm.Chunk{ToolCall: part}, nil) {
					return
				}
			}
		}

		yield(llm.Chunk{Done: true, FinishReason: mapStop(acc.StopReason), Usage: usageFrom(acc.Usage)}, nil)
	}
}

// ModelEntry is one entry of the account's model catalog (/v1/models) — the
// subset a front-end needs (picker rows, model-ref validation).
type ModelEntry struct {
	ID             string
	DisplayName    string
	MaxInputTokens int
}

// ListModels fetches the models this API key can use, as the API orders them
// (newest first). It exists so front-ends can VALIDATE a typed model id and
// offer a picker instead of discovering a typo as a mid-run 400.
func (p *Provider) ListModels(ctx context.Context) ([]ModelEntry, error) {
	var out []ModelEntry
	it := p.client.Models.ListAutoPaging(ctx, sdk.ModelListParams{})
	for it.Next() {
		m := it.Current()
		out = append(out, ModelEntry{ID: m.ID, DisplayName: m.DisplayName, MaxInputTokens: int(m.MaxInputTokens)})
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("llm: %s: list models: %w", p.name, err)
	}
	return out, nil
}

// buildParams maps a provider-agnostic Request onto the Messages API params,
// shared verbatim by Generate and Stream so the two paths can never drift.
func (p *Provider) buildParams(req llm.Request) (sdk.MessageNewParams, []option.RequestOption) {
	model := req.Model
	if model == "" {
		model = p.model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.maxTokens // the wire REQUIRES max_tokens; 0 means "don't generate".
	}

	params := sdk.MessageNewParams{
		Model:     sdk.Model(model),
		MaxTokens: int64(maxTokens),
		System:    toSystem(req),
		Messages:  toMessages(req.Messages),
	}
	if p.cache {
		// (HP-8) Two anchors, same shape as openaicompat's breakpoints: the
		// stable system prefix and the last transcript block.
		if n := len(params.System); n > 0 {
			params.System[n-1].CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		markTailCacheControl(params.Messages)
	}
	if req.Temperature != nil && !isClaude5(model) {
		// Claude 5 models REJECT temperature outright ("`temperature` is
		// deprecated for this model", 400) — and the agent loop pins
		// temperature on every run, so sending it would brick every 5-family
		// call. Dropped for the 5 family only; 4.x and older still honor it.
		params.Temperature = sdk.Float(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = sdk.Float(*req.TopP)
	}
	// req.Seed: the Messages API has no seed parameter; deliberately dropped.
	if len(req.Stop) > 0 {
		params.StopSequences = req.Stop
	}
	if tools := toTools(req.Tools); len(tools) > 0 {
		params.Tools = tools
	}
	if req.ReasoningEffort != "" {
		// Native effort knob (low|medium|high|xhigh|max on effort-capable
		// models). Passed through untyped per llm.Request's contract; an
		// unsupported value/model is the API's visible error, not a silent drop.
		params.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(req.ReasoningEffort)}
	}

	reqOpts := make([]option.RequestOption, 0, len(req.ProviderOptions))
	for k, v := range req.ProviderOptions {
		reqOpts = append(reqOpts, option.WithJSONSet(k, v))
	}
	return params, reqOpts
}

// isClaude5 reports whether a model id names a Claude 5-family model
// (claude-fable-5, claude-mythos-5, a dated claude-<name>-5-YYYYMMDD, …) —
// the tier that dropped sampling knobs (temperature) and is adaptive-thinking
// only. Matched structurally so new 5-family names need no code change.
func isClaude5(model string) bool {
	rest, ok := strings.CutPrefix(model, "claude-")
	if !ok {
		return false
	}
	// The tier digit follows an ALPHABETIC family name: "fable-5",
	// "sonnet-5-20260101". A digit-led segment is the legacy tier-first
	// naming ("claude-3-5-sonnet-…"), which is not the 5 family.
	i := strings.IndexByte(rest, '-')
	if i <= 0 {
		return false
	}
	for _, r := range rest[:i] {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	v := rest[i+1:]
	return strings.HasPrefix(v, "5") && (len(v) == 1 || v[1] == '-' || v[1] == '.')
}

func markTailCacheControl(msgs []sdk.MessageParam) {
	for i := len(msgs) - 1; i >= 0; i-- {
		for j := len(msgs[i].Content) - 1; j >= 0; j-- {
			b := msgs[i].Content[j]
			switch {
			case b.OfText != nil:
				marked := *b.OfText
				marked.CacheControl = sdk.NewCacheControlEphemeralParam()
				msgs[i].Content[j] = sdk.ContentBlockParamUnion{OfText: &marked}
				return
			case b.OfToolResult != nil:
				marked := *b.OfToolResult
				marked.CacheControl = sdk.NewCacheControlEphemeralParam()
				msgs[i].Content[j] = sdk.ContentBlockParamUnion{OfToolResult: &marked}
				return
			case b.OfToolUse != nil:
				marked := *b.OfToolUse
				marked.CacheControl = sdk.NewCacheControlEphemeralParam()
				msgs[i].Content[j] = sdk.ContentBlockParamUnion{OfToolUse: &marked}
				return
			}
		}
	}
}

// toSystem folds Request.System plus any RoleSystem messages into the
// top-level system blocks — the Messages API has no system role in the
// conversation; system context is a top-level field.
func toSystem(req llm.Request) []sdk.TextBlockParam {
	var out []sdk.TextBlockParam
	if req.System != "" {
		out = append(out, sdk.TextBlockParam{Text: req.System})
	}
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem {
			if txt := m.Text(); txt != "" {
				out = append(out, sdk.TextBlockParam{Text: txt})
			}
		}
	}
	return out
}

// toMessages translates our message model into Messages API turns:
//
//   - assistant turns replay thinking blocks FIRST (signed, verbatim — the API
//     rejects a tool-using assistant turn whose thinking is missing or moved),
//     then text, then tool_use blocks;
//   - a RoleTool message becomes a USER turn of tool_result blocks (there is
//     no tool role on this wire), which natively carries IsError;
//   - RoleSystem is handled by toSystem, skipped here.
func toMessages(msgs []llm.Message) []sdk.MessageParam {
	out := make([]sdk.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			continue
		case llm.RoleAssistant:
			if blocks := assistantBlocks(m); len(blocks) > 0 {
				out = append(out, sdk.NewAssistantMessage(blocks...))
			}
		case llm.RoleTool:
			var blocks []sdk.ContentBlockParamUnion
			for _, part := range m.Parts {
				if tr, ok := part.(llm.ToolResultPart); ok {
					blocks = append(blocks, sdk.NewToolResultBlock(tr.ToolCallID, tr.Content, tr.IsError))
				}
			}
			if len(blocks) > 0 {
				out = append(out, sdk.NewUserMessage(blocks...))
			}
		default: // user, anything else
			if blocks := userBlocks(m); len(blocks) > 0 {
				out = append(out, sdk.NewUserMessage(blocks...))
			}
		}
	}
	return out
}

// assistantBlocks rebuilds an assistant turn's content blocks in the order the
// API requires: thinking (replayed verbatim from ReasoningPart.Raw), text,
// tool_use. An empty turn yields no blocks and the caller skips the message
// (the API rejects an empty content array).
func assistantBlocks(m llm.Message) []sdk.ContentBlockParamUnion {
	var thinking, calls []sdk.ContentBlockParamUnion
	for _, part := range m.Parts {
		switch tp := part.(type) {
		case llm.ReasoningPart:
			if b, ok := thinkingBlockFromRaw(tp.Raw); ok {
				thinking = append(thinking, b)
			}
		case llm.ToolCallPart:
			calls = append(calls, sdk.NewToolUseBlock(tp.ID, rawOrEmptyObject(tp.Args), tp.Name))
		}
	}
	blocks := thinking
	if txt := m.Text(); txt != "" {
		blocks = append(blocks, sdk.NewTextBlock(txt))
	}
	return append(blocks, calls...)
}

// userBlocks builds content blocks for a user message from ordered parts.
// Text-only messages produce a single text block; messages with images produce
// text + image blocks in order. ImageURL parts are not supported on the
// Anthropic wire (the SDK has no URLImageSourceParam constructor for base
// messages), so they are silently skipped with a log note.
func userBlocks(m llm.Message) []sdk.ContentBlockParamUnion {
	hasImage := false
	for _, p := range m.Parts {
		if _, ok := p.(llm.ImagePart); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		if txt := m.Text(); txt != "" {
			return []sdk.ContentBlockParamUnion{sdk.NewTextBlock(txt)}
		}
		return nil
	}

	var blocks []sdk.ContentBlockParamUnion
	for _, p := range m.Parts {
		switch tp := p.(type) {
		case llm.TextPart:
			if tp.Text != "" {
				blocks = append(blocks, sdk.NewTextBlock(tp.Text))
			}
		case llm.ImagePart:
			if tp.URL != "" {
				// Anthropic Messages API has no URL-based image source for
				// base messages (only for PDFs/documents via beta). Skip
				// with a diagnostic note rather than silently dropping.
				fmt.Fprintf(os.Stderr, "anthropic: ImagePart URL not supported, skipping\n")
				continue
			}
			blocks = append(blocks, sdk.NewImageBlockBase64(tp.MIME, base64.StdEncoding.EncodeToString(tp.Data)))
		}
	}
	return blocks
}

// thinkingBlockFromRaw reconstructs a thinking / redacted_thinking param block
// from the raw response block captured in ReasoningPart.Raw. The signature is
// replayed byte-for-byte; the harness never interprets it (llm.ReasoningPart's
// contract). An unrecognized payload is dropped — the API treats replayed
// garbage as an error, absence as "this turn had no thinking".
func thinkingBlockFromRaw(raw json.RawMessage) (sdk.ContentBlockParamUnion, bool) {
	var b struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return sdk.ContentBlockParamUnion{}, false
	}
	switch b.Type {
	case "thinking":
		return sdk.NewThinkingBlock(b.Signature, b.Thinking), true
	case "redacted_thinking":
		return sdk.NewRedactedThinkingBlock(b.Data), true
	default:
		return sdk.ContentBlockParamUnion{}, false
	}
}

// rawOrEmptyObject guards replayed tool-call args: the wire field must be a
// valid JSON value, and a no-arg call's empty Args must serialize as "{}".
func rawOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if strings.TrimSpace(string(raw)) == "" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// toTools translates provider-agnostic tools into Messages API tool params.
// Schema is a JSON Schema object; its fields map onto input_schema (which is
// itself just a JSON Schema with a fixed "object" type).
func toTools(tools []llm.Tool) []sdk.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]sdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		tp := sdk.ToolParam{Name: t.Name}
		if t.Description != "" {
			tp.Description = sdk.String(t.Description)
		}
		if len(t.Schema) > 0 {
			var schema struct {
				Properties json.RawMessage `json:"properties"`
				Required   []string        `json:"required"`
			}
			if json.Unmarshal(t.Schema, &schema) == nil {
				if len(schema.Properties) > 0 {
					var props any
					if json.Unmarshal(schema.Properties, &props) == nil {
						tp.InputSchema.Properties = props
					}
				}
				tp.InputSchema.Required = schema.Required
			}
		}
		out = append(out, sdk.ToolUnionParam{OfTool: &tp})
	}
	return out
}

// toResponse maps a Messages API response onto our normalized Response,
// preserving block order. Thinking blocks (signed) and redacted thinking ride
// as ReasoningPart carrying the RAW block JSON, so the loop can replay them
// verbatim on the next turn — same mechanism that keeps Gemini's encrypted
// signatures alive through openaicompat.
func toResponse(msg *sdk.Message) *llm.Response {
	r := &llm.Response{Model: string(msg.Model), Raw: msg, FinishReason: mapStop(msg.StopReason), Usage: usageFrom(msg.Usage)}
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			r.Content = append(r.Content, llm.Text(block.Text))
		case "thinking", "redacted_thinking":
			r.Content = append(r.Content, llm.ReasoningPart{Raw: json.RawMessage(block.RawJSON())})
		case "tool_use":
			r.Content = append(r.Content, llm.ToolCallPart{ID: block.ID, Name: block.Name, Args: rawOrEmptyObject(block.Input)})
		}
	}
	return r
}

// usageFrom normalizes Anthropic token accounting. The wire's input_tokens
// EXCLUDES cached tokens (cache reads/writes are separate fields), while our
// Usage follows OpenAI semantics — PromptTokens is the whole prompt and
// CachedTokens a subset of it — so the three input fields are summed.
func usageFrom(u sdk.Usage) llm.Usage {
	prompt := int(u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens)
	return llm.Usage{
		PromptTokens:     prompt,
		CompletionTokens: int(u.OutputTokens),
		TotalTokens:      prompt + int(u.OutputTokens),
		CachedTokens:     int(u.CacheReadInputTokens),
		ReasoningTokens:  int(u.OutputTokensDetails.ThinkingTokens),
	}
}

func mapStop(reason sdk.StopReason) llm.FinishReason {
	switch reason {
	case sdk.StopReasonEndTurn, sdk.StopReasonStopSequence, sdk.StopReasonPauseTurn:
		// pause_turn means "long-running turn paused, re-send to continue"; the
		// loop re-sends everything anyway, so it terminates like a stop.
		return llm.FinishStop
	case sdk.StopReasonMaxTokens:
		return llm.FinishLength
	case sdk.StopReasonToolUse:
		return llm.FinishToolUse
	case sdk.StopReasonRefusal:
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

	// NOTE: this branch must run before any err.Error() call — sdk.Error's
	// Error() dereferences the attached *http.Request, and classify must not
	// assume one is present.
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		kind := llm.KindUnknown
		retryable := false
		// RawJSON is the API's error body ({"type":"error","error":{"type":...}}),
		// safe to read without a request attached.
		raw := strings.ToLower(apiErr.RawJSON())
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = llm.KindAuth
		case http.StatusTooManyRequests:
			kind = llm.KindRateLimit
			retryable = true
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			if isContextLength(raw) {
				kind = llm.KindContextLength
			}
		case http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout,
			529: // Anthropic "overloaded_error" — transient capacity, retry.
			retryable = true
		case http.StatusOK:
			// An SSE `error` EVENT after the request was accepted: the SDK's
			// ssestream folds it into an API error still carrying the 200
			// response. The attempt died mid-stream, which is transient by
			// default — a streamed error never commits the turn, so the agent's
			// retry re-send is safe (same policy as openaicompat) — EXCEPT the
			// non-transient shapes below.
			retryable = true
			switch {
			case isContextLength(raw):
				kind, retryable = llm.KindContextLength, false
			case strings.Contains(raw, "authentication_error") || strings.Contains(raw, "permission_error"):
				kind, retryable = llm.KindAuth, false
			case strings.Contains(raw, "rate_limit_error"):
				kind = llm.KindRateLimit
			}
		}
		return &llm.ProviderError{
			Provider:   p.name,
			Kind:       kind,
			StatusCode: apiErr.StatusCode,
			Retryable:  retryable,
			Err:        err,
		}
	}

	// Fallback for mid-stream failures the SDK could NOT parse into an API
	// error: a malformed error event surfaces as fmt.Errorf("received error
	// while streaming: %s", data), a dropped connection as ErrUnexpectedEOF.
	// Both are transient.
	if strings.Contains(err.Error(), "received error while streaming: ") || errors.Is(err, io.ErrUnexpectedEOF) {
		return &llm.ProviderError{Provider: p.name, Kind: llm.KindUnknown, Retryable: true, Err: err}
	}

	return &llm.ProviderError{Provider: p.name, Kind: llm.KindUnknown, Err: err}
}

func isContextLength(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "prompt is too long") ||
		strings.Contains(m, "context limit") ||
		strings.Contains(m, "maximum context") ||
		strings.Contains(m, "too many tokens")
}
