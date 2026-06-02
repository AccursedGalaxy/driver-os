package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// newTestProvider points the adapter at an httptest server so Generate runs
// the full request/parse path without touching the network.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{Name: "test", BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})
}

func TestGenerateToolCallRoundTrip(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id":"chatcmpl-2","model":"m",
			"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"arg\":\"go.mod\"}"}}]}}],
			"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}
		}`)
	})

	schema := json.RawMessage(`{"type":"object","properties":{"arg":{"type":"string"}},"required":["arg"]}`)
	resp, err := p.Generate(context.Background(), llm.Request{
		Messages: []llm.Message{
			llm.User("read it"),
			// a prior tool-calling turn + its result, to prove the round-trip serializes.
			{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.ToolCallPart{ID: "call_0", Name: "read_file", Args: json.RawMessage(`{"arg":"x"}`)}}},
			llm.ToolResultMsg("call_0", "1| module x", false),
		},
		Tools: []llm.Tool{{Name: "read_file", Description: "read a file", Schema: schema}},
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	// --- outgoing request carried the tool schema + the round-trip messages ---
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("request tools = %v, want 1", gotBody["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read_file" || fn["parameters"] == nil {
		t.Errorf("tool function = %v, want name=read_file with parameters", fn)
	}
	var sawToolMsg, sawAsstCall bool
	for _, mm := range gotBody["messages"].([]any) {
		m := mm.(map[string]any)
		if m["role"] == "tool" && m["tool_call_id"] == "call_0" {
			sawToolMsg = true
		}
		if m["role"] == "assistant" {
			if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 {
				sawAsstCall = true
			}
		}
	}
	if !sawToolMsg {
		t.Errorf("outgoing messages lack the tool result for call_0")
	}
	if !sawAsstCall {
		t.Errorf("outgoing messages lack the assistant tool_calls turn")
	}

	// --- parsed response surfaced the tool call ---
	if resp.FinishReason != llm.FinishToolUse {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishToolUse)
	}
	var calls int
	for _, part := range resp.Content {
		if tc, ok := part.(llm.ToolCallPart); ok {
			calls++
			if tc.Name != "read_file" || tc.ID != "call_1" || string(tc.Args) != `{"arg":"go.mod"}` {
				t.Errorf("parsed call = %+v, want read_file/call_1/{\"arg\":\"go.mod\"}", tc)
			}
		}
	}
	if calls != 1 {
		t.Errorf("parsed %d tool calls, want 1", calls)
	}
}

func TestGenerateParsesResponse(t *testing.T) {
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id": "chatcmpl-1",
			"model": "test-model-resolved",
			"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello there"}}],
			"usage": {"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":4}}
		}`)
	})

	temp := 0.5
	resp, err := p.Generate(context.Background(), llm.Request{
		System:      "be terse",
		Messages:    []llm.Message{llm.User("hi")},
		MaxTokens:   200,
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	if resp.Text() != "hello there" {
		t.Errorf("Text = %q, want %q", resp.Text(), "hello there")
	}
	if resp.Model != "test-model-resolved" {
		t.Errorf("Model = %q, want resolved model from response", resp.Model)
	}
	if resp.FinishReason != llm.FinishStop {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, llm.FinishStop)
	}
	want := llm.Usage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14, CachedTokens: 4}
	if resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
	if resp.Raw == nil {
		t.Error("Raw escape hatch should be populated")
	}

	// Request shaping: default model used, system prepended, sampling applied.
	if gotBody["model"] != "test-model" {
		t.Errorf("request model = %v, want default test-model", gotBody["model"])
	}
	if gotBody["temperature"] != 0.5 {
		t.Errorf("request temperature = %v, want 0.5", gotBody["temperature"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2 (system + user)", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("first message role = %v, want system", first["role"])
	}
}

func TestGenerateUsesRequestModelOverride(t *testing.T) {
	var gotModel any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotModel = body["model"]
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{}}`)
	})
	if _, err := p.Generate(context.Background(), llm.Request{
		Model:    "override-model",
		Messages: []llm.Message{llm.User("hi")},
	}); err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if gotModel != "override-model" {
		t.Errorf("request model = %v, want override-model", gotModel)
	}
}

func TestClassifyRateLimit(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limited"}}`)
	})
	_, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
	if !errors.Is(err, llm.ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	var perr *llm.ProviderError
	if !errors.As(err, &perr) || !perr.Retryable || perr.StatusCode != 429 || perr.Provider != "test" {
		t.Fatalf("ProviderError = %+v, want retryable rate-limit (429) from provider 'test'", perr)
	}
}

func TestClassifyAuth(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key","type":"auth_error"}}`)
	})
	_, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
	if !errors.Is(err, llm.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestClassifyContextLength(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error"}}`)
	})
	_, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
	if !errors.Is(err, llm.ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength", err)
	}
}

func TestMapFinish(t *testing.T) {
	cases := map[string]llm.FinishReason{
		"stop":           llm.FinishStop,
		"length":         llm.FinishLength,
		"tool_calls":     llm.FinishToolUse,
		"content_filter": llm.FinishContentFilter,
		"weird":          llm.FinishOther,
	}
	for in, want := range cases {
		if got := mapFinish(in); got != want {
			t.Errorf("mapFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderImplementsInterface(t *testing.T) {
	var _ llm.Provider = OpenRouter("openai/gpt-4o-mini")
	var _ llm.Provider = XAI("grok-3-mini")
	if !strings.EqualFold(OpenAI("gpt-4o").Name(), "openai") {
		t.Error("preset name mismatch")
	}
}

func TestCapabilities(t *testing.T) {
	// Cloud presets advertise native tools but NOT vision (vision isn't wired
	// through the adapter yet — advertising it would be a lie the loop trusts).
	for _, p := range []*Provider{OpenAI("gpt-4o"), XAI("grok-4"), OpenRouter("x/y")} {
		c := p.Capabilities()
		if !c.Tools {
			t.Errorf("%s: Tools = false, want true", p.Name())
		}
		if c.Vision {
			t.Errorf("%s: Vision = true, but vision is not wired through the adapter", p.Name())
		}
	}

	// Ollama defaults to NO native tools so the loop falls back to the text
	// protocol (local tool support is per-model); a real run opts in via Config.
	if Ollama("llama3").Capabilities().Tools {
		t.Error("Ollama default Capabilities().Tools = true, want false (text-loop fallback)")
	}

	// An explicit Config override wins over the constructor default.
	custom := New(Config{Name: "vllm", BaseURL: "http://x", Model: "m", Capabilities: &llm.Capabilities{Tools: true, Vision: true}})
	if c := custom.Capabilities(); !c.Tools || !c.Vision {
		t.Errorf("override Capabilities = %+v, want Tools && Vision", c)
	}
}
