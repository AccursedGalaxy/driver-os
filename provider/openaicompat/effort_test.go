package openaicompat

// llm.Request.ReasoningEffort must reach the wire as `reasoning_effort` (the
// OpenAI chat-completions param, which OpenRouter folds into its unified
// reasoning config) — and stay OFF the wire when unset, so providers without
// a reasoning surface never see an unknown value.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func captureEffortBody(t *testing.T) (*Provider, *map[string]any) {
	t.Helper()
	var gotBody map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
	})
	return p, &gotBody
}

func TestReasoningEffortOnWire(t *testing.T) {
	p, body := captureEffortBody(t)
	_, err := p.Generate(context.Background(), llm.Request{
		Messages:        []llm.Message{llm.User("hi")},
		ReasoningEffort: "xhigh",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := (*body)["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort on wire = %v, want xhigh", got)
	}
}

func TestReasoningEffortOmittedWhenUnset(t *testing.T) {
	p, body := captureEffortBody(t)
	if _, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if v, present := (*body)["reasoning_effort"]; present {
		t.Fatalf("reasoning_effort should be absent when unset, got %v", v)
	}
}
