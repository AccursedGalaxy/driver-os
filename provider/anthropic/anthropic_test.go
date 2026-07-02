package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// newTestProvider points the adapter at an httptest server so Generate runs
// the full request/parse path without touching the network.
func newTestProvider(t *testing.T, cfg Config, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg.Name = "test"
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	if cfg.Model == "" {
		cfg.Model = "test-model"
	}
	return New(cfg)
}

func TestGenerateToolCallRoundTrip(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"arg":"go.mod"}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}
		}`)
	})

	schema := json.RawMessage(`{"type":"object","properties":{"arg":{"type":"string"}},"required":["arg"]}`)
	resp, err := p.Generate(context.Background(), llm.Request{
		System: "be terse",
		Messages: []llm.Message{
			llm.User("read it"),
			// a prior tool-calling turn + its result, to prove the round-trip serializes.
			{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.ToolCallPart{ID: "toolu_0", Name: "read_file", Args: json.RawMessage(`{"arg":"x"}`)}}},
			llm.ToolResultMsg("toolu_0", "1| module x", true),
		},
		Tools: []llm.Tool{{Name: "read_file", Description: "read a file", Schema: schema}},
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	// --- outgoing request: system rides top-level, tools carry input_schema ---
	sys, _ := gotBody["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "be terse" {
		t.Errorf("system = %v, want one 'be terse' block", gotBody["system"])
	}
	if gotBody["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want the %d default (the wire requires a value)", gotBody["max_tokens"], DefaultMaxTokens)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("request tools = %v, want 1", gotBody["tools"])
	}
	tool := tools[0].(map[string]any)
	is, _ := tool["input_schema"].(map[string]any)
	if tool["name"] != "read_file" || is["properties"] == nil || is["type"] != "object" {
		t.Errorf("tool = %v, want name=read_file with an object input_schema", tool)
	}

	// --- the tool result must be a USER message of tool_result blocks (no tool
	// role on this wire), carrying is_error natively ---
	var sawToolResult, sawAsstCall bool
	for _, mm := range gotBody["messages"].([]any) {
		m := mm.(map[string]any)
		blocks, _ := m["content"].([]any)
		for _, bb := range blocks {
			b := bb.(map[string]any)
			if b["type"] == "tool_result" && m["role"] == "user" {
				if b["tool_use_id"] != "toolu_0" || b["is_error"] != true {
					t.Errorf("tool_result block = %v, want tool_use_id=toolu_0 is_error=true", b)
				}
				sawToolResult = true
			}
			if b["type"] == "tool_use" && m["role"] == "assistant" {
				sawAsstCall = true
			}
		}
	}
	if !sawToolResult {
		t.Errorf("outgoing messages lack the user-role tool_result for toolu_0")
	}
	if !sawAsstCall {
		t.Errorf("outgoing messages lack the assistant tool_use turn")
	}

	// --- parsed response surfaced the tool call ---
	if resp.FinishReason != llm.FinishToolUse {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishToolUse)
	}
	var calls int
	for _, part := range resp.Content {
		if tc, ok := part.(llm.ToolCallPart); ok {
			calls++
			if tc.Name != "read_file" || tc.ID != "toolu_1" || string(tc.Args) != `{"arg":"go.mod"}` {
				t.Errorf("parsed call = %+v, want read_file/toolu_1/{\"arg\":\"go.mod\"}", tc)
			}
		}
	}
	if calls != 1 {
		t.Errorf("parsed %d tool calls, want 1", calls)
	}
}

// TestThinkingRoundTrip locks in signed-thinking replay: a thinking model
// returns a signed thinking block, the adapter must (1) surface it as an
// llm.ReasoningPart carrying the RAW block and (2) replay it VERBATIM as the
// FIRST block of the re-sent assistant turn — the API rejects a tool-using
// assistant turn whose thinking blocks are missing, altered, or reordered.
func TestThinkingRoundTrip(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id":"msg_2","type":"message","role":"assistant","model":"claude-test",
			"content":[
				{"type":"thinking","thinking":"let me check","signature":"SIG_BLOB"},
				{"type":"tool_use","id":"toolu_1","name":"run","input":{"command":"go test"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":5,"output_tokens":9,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,
				"output_tokens_details":{"thinking_tokens":6}}
		}`)
	})

	// (1) parse: the response surfaces a ReasoningPart carrying the raw block.
	resp, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("test it")}})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	var reasoning []llm.ReasoningPart
	for _, part := range resp.Content {
		if rp, ok := part.(llm.ReasoningPart); ok {
			reasoning = append(reasoning, rp)
		}
	}
	if len(reasoning) != 1 || !strings.Contains(string(reasoning[0].Raw), "SIG_BLOB") {
		t.Fatalf("ReasoningParts = %v, want one carrying SIG_BLOB", reasoning)
	}
	if resp.Usage.ReasoningTokens != 6 {
		t.Errorf("ReasoningTokens = %d, want 6", resp.Usage.ReasoningTokens)
	}

	// (2) replay: re-send the turn (reasoning part deliberately placed AFTER
	// the tool call, as a loop that appends parts in arrival order might);
	// the wire message must still lead with the verbatim thinking block.
	_, err = p.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{
			llm.User("test it"),
			{Role: llm.RoleAssistant, Parts: []llm.ContentPart{
				llm.ToolCallPart{ID: "toolu_1", Name: "run", Args: json.RawMessage(`{"command":"go test"}`)},
				reasoning[0],
			}},
			llm.ToolResultMsg("toolu_1", "ok", false),
		},
	})
	if err != nil {
		t.Fatalf("Generate (replay) error: %v", err)
	}
	msgs := gotBody["messages"].([]any)
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("replayed assistant blocks = %v, want [thinking, tool_use]", blocks)
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "thinking" || first["signature"] != "SIG_BLOB" || first["thinking"] != "let me check" {
		t.Errorf("first replayed block = %v, want the verbatim signed thinking block", first)
	}
	if blocks[1].(map[string]any)["type"] != "tool_use" {
		t.Errorf("second replayed block = %v, want tool_use", blocks[1])
	}
}

func TestBuildParamsKnobs(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, Config{PromptCache: true}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_3","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":100,"output_tokens":3,"cache_read_input_tokens":40,"cache_creation_input_tokens":10}}`)
	})

	temp := 0.2
	resp, err := p.Generate(context.Background(), llm.Request{
		System:          "sys",
		Messages:        []llm.Message{llm.User("hi")},
		MaxTokens:       512,
		Temperature:     &temp,
		Stop:            []string{"END"},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	if gotBody["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v, want 512 (explicit beats default)", gotBody["max_tokens"])
	}
	if gotBody["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want 0.2", gotBody["temperature"])
	}
	if stops, _ := gotBody["stop_sequences"].([]any); len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop_sequences = %v, want [END]", gotBody["stop_sequences"])
	}
	oc, _ := gotBody["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Errorf("output_config = %v, want effort=high (the native effort knob)", gotBody["output_config"])
	}

	// PromptCache marks the two anchors: the system block and the request tail
	// (top-level cache_control, which the API applies to the last cacheable block).
	sys := gotBody["system"].([]any)[0].(map[string]any)
	if sys["cache_control"] == nil {
		t.Errorf("system block lacks cache_control: %v", sys)
	}
	if gotBody["cache_control"] == nil {
		t.Errorf("request lacks the top-level tail cache_control")
	}

	// Usage normalization: Anthropic's input_tokens EXCLUDES cache reads/writes;
	// ours is the whole prompt with CachedTokens a subset.
	want := llm.Usage{PromptTokens: 150, CompletionTokens: 3, TotalTokens: 153, CachedTokens: 40}
	if resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestStream(t *testing.T) {
	p := newTestProvider(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_4","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm, a test"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG_"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"BLOB"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"run","input":{}}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"go test\"}"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":2}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			io.WriteString(w, ev+"\n\n")
		}
	})

	var text strings.Builder
	var calls []*llm.ToolCallPart
	var reasoning []*llm.ReasoningPart
	var done *llm.Chunk
	for chunk, err := range p.Stream(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch {
		case chunk.Done:
			c := chunk
			done = &c
		case chunk.Reasoning != nil:
			reasoning = append(reasoning, chunk.Reasoning)
		case chunk.ToolCall != nil:
			calls = append(calls, chunk.ToolCall)
		default:
			text.WriteString(chunk.Text)
		}
	}

	if text.String() != "Hello" {
		t.Errorf("streamed text = %q, want %q (thinking deltas must NOT leak into text)", text.String(), "Hello")
	}
	if len(calls) != 1 || calls[0].Name != "run" || string(calls[0].Args) != `{"command":"go test"}` {
		t.Errorf("assembled calls = %+v, want one run/{\"command\":\"go test\"}", calls)
	}
	// The assembled thinking block must ride out as a Reasoning chunk carrying
	// the FULL accumulated signature, and replay back as a valid thinking block
	// — a streamed turn is re-sent just like a Generate one.
	if len(reasoning) != 1 || !strings.Contains(string(reasoning[0].Raw), "SIG_BLOB") {
		t.Fatalf("reasoning chunks = %v, want one carrying the assembled SIG_BLOB signature", reasoning)
	}
	if b, ok := thinkingBlockFromRaw(reasoning[0].Raw); !ok || b.OfThinking == nil || b.OfThinking.Signature != "SIG_BLOB" || b.OfThinking.Thinking != "hmm, a test" {
		t.Errorf("streamed reasoning does not replay as a thinking block: %s", reasoning[0].Raw)
	}
	if done == nil {
		t.Fatal("no terminal Done chunk")
	}
	if done.FinishReason != llm.FinishToolUse {
		t.Errorf("Done.FinishReason = %q, want %q", done.FinishReason, llm.FinishToolUse)
	}
	if done.Usage.CompletionTokens != 7 || done.Usage.PromptTokens != 10 {
		t.Errorf("Done.Usage = %+v, want prompt=10 completion=7", done.Usage)
	}
}

// TestStreamErrorEventRetryable: an SSE `error` event after the request was
// accepted (e.g. overloaded_error) must classify as retryable — a streamed
// error never commits the turn, so the agent's re-send is safe.
func TestStreamErrorEventRetryable(t *testing.T) {
	p := newTestProvider(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_5","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
		io.WriteString(w, `event: error`+"\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n")
	})

	var gotErr error
	for _, err := range p.Stream(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}}) {
		if err != nil {
			gotErr = err
		}
	}
	var perr *llm.ProviderError
	if !errors.As(gotErr, &perr) {
		t.Fatalf("stream error = %v, want a *llm.ProviderError", gotErr)
	}
	if !perr.Retryable {
		t.Errorf("overloaded_error mid-stream classified Retryable=false, want true: %v", perr)
	}
}

func TestClassify(t *testing.T) {
	// 401 through the real HTTP path (not retried by the SDK, so fast).
	p := newTestProvider(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	})
	_, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
	if !errors.Is(err, llm.ErrAuth) {
		t.Errorf("401 classified as %v, want KindAuth", err)
	}

	// Statuses the SDK would retry with backoff (429/529) are classified from
	// constructed errors to keep the test fast.
	if perr := p.classify(&sdk.Error{StatusCode: 529}).(*llm.ProviderError); !perr.Retryable {
		t.Errorf("529 overloaded: Retryable=false, want true")
	}
	perr := p.classify(&sdk.Error{StatusCode: http.StatusTooManyRequests}).(*llm.ProviderError)
	if perr.Kind != llm.KindRateLimit || !perr.Retryable {
		t.Errorf("429 = %+v, want retryable KindRateLimit", perr)
	}
	if !errors.Is(p.classify(context.Canceled), llm.ErrCanceled) {
		t.Errorf("context.Canceled not classified as KindCanceled")
	}
}

// TestContextLengthNotRetryable: a window overflow must route to the eviction
// fallback, never to a verbatim (still overlong) retry.
func TestContextLengthNotRetryable(t *testing.T) {
	p := newTestProvider(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`)
	})
	_, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
	var perr *llm.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %v, want a *llm.ProviderError", err)
	}
	if perr.Kind != llm.KindContextLength || perr.Retryable {
		t.Errorf("overflow = %+v, want non-retryable KindContextLength", perr)
	}
}

// Claude 5 models reject `temperature` outright (400 "deprecated for this
// model") — and the agent loop pins temperature on every run, so the adapter
// must DROP it for the 5 family while still honoring it on 4.x and older.
func TestTemperatureDroppedForClaude5(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, Config{Model: "claude-fable-5"}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_5","type":"message","role":"assistant","model":"claude-fable-5",
			"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":5,"output_tokens":1}}`)
	})
	temp := 0.0
	if _, err := p.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, Temperature: &temp,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, present := gotBody["temperature"]; present {
		t.Errorf("temperature sent to a Claude 5 model: %v", gotBody["temperature"])
	}
}

func TestIsClaude5(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-fable-5", true},
		{"claude-mythos-5", true},
		{"claude-sonnet-5-20260101", true},
		{"claude-fable-5.5", true},
		{"claude-haiku-4-5", false}, // 4.5, not the 5 family.
		{"claude-sonnet-4-6", false},
		{"claude-opus-4-8", false},
		{"claude-3-5-sonnet-20241022", false},
		{"claude-fable-50", false},
		{"gpt-5.5", false},
	}
	for _, c := range cases {
		if got := isClaude5(c.model); got != c.want {
			t.Errorf("isClaude5(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

// ListModels must surface the account catalog as picker/validation-ready
// entries (the /v1/models page shape).
func TestListModels(t *testing.T) {
	p := newTestProvider(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[
			{"id":"claude-fable-5","display_name":"Claude Fable 5","created_at":"2026-06-01T00:00:00Z","max_input_tokens":500000,"capabilities":{}},
			{"id":"claude-haiku-4-5","display_name":"Claude Haiku 4.5","created_at":"2025-10-01T00:00:00Z","max_input_tokens":200000,"capabilities":{}}],
			"has_more":false,"first_id":"claude-fable-5","last_id":"claude-haiku-4-5"}`)
	})
	ms, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].ID != "claude-fable-5" || ms[0].DisplayName != "Claude Fable 5" || ms[0].MaxInputTokens != 500000 {
		t.Fatalf("models = %+v", ms)
	}
}
