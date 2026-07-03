package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestPinning(t *testing.T) {
	t.Run("hasEncryptedReasoning", func(t *testing.T) {
		tests := []struct {
			raw  string
			want bool
		}{
			{`[]`, false},
			{`[{"format":"text"}]`, false},
			{`[{"format":"google-gemini-v1"}]`, true},
			{`[{"format":"text"},{"format":"google-gemini-v1"}]`, true},
			{`invalid`, false},
		}
		for _, tt := range tests {
			if got := hasEncryptedReasoning(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("hasEncryptedReasoning(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		}
	})

	t.Run("pinProviderName", func(t *testing.T) {
		tests := []struct {
			display string
			want    string
		}{
			{"Google AI Studio", "google-ai-studio"},
			{"Google", "google-vertex"},
			{"Unknown", ""},
			{"", ""},
		}
		for _, tt := range tests {
			if got := pinProviderName(tt.display); got != tt.want {
				t.Errorf("pinProviderName(%q) = %q, want %q", tt.display, got, tt.want)
			}
		}
	})

	t.Run("StickyPinning", func(t *testing.T) {
		t.Setenv("OPENROUTER_PIN_PROVIDER", "")
		var lastBody map[string]any
		handler := func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			lastBody = nil
			_ = json.Unmarshal(body, &lastBody)
			w.Header().Set("Content-Type", "application/json")
			// Return a response with reasoning_details and the provider field
			io.WriteString(w, `{
				"id":"x","model":"m",
				"choices":[{"index":0,"message":{
					"role":"assistant","content":"hi",
					"reasoning_details":[{"format":"google-gemini-v1"}]
				}}],
				"provider":"Google AI Studio"
			}`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "key"})

		// First request: no pinning yet
		_, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := lastBody["provider"]; ok {
			t.Errorf("first request should not have provider field, got %v", lastBody["provider"])
		}

		// Second request: should be pinned to google-ai-studio
		_, err = p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi again")}})
		if err != nil {
			t.Fatal(err)
		}
		prov, ok := lastBody["provider"].(map[string]any)
		if !ok {
			t.Fatalf("second request missing provider field: %v", lastBody)
		}
		order := prov["order"].([]any)
		if len(order) != 1 || order[0] != "google-ai-studio" {
			t.Errorf("provider.order = %v, want [google-ai-studio]", order)
		}
		if prov["allow_fallbacks"] != false {
			t.Errorf("allow_fallbacks = %v, want false", prov["allow_fallbacks"])
		}
	})

	t.Run("NoPinningWithoutEncryptedReasoning", func(t *testing.T) {
		t.Setenv("OPENROUTER_PIN_PROVIDER", "")
		var lastBody map[string]any
		handler := func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			lastBody = nil
			_ = json.Unmarshal(body, &lastBody)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{
				"id":"x","model":"m",
				"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],
				"provider":"Google AI Studio"
			}`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "key"})

		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi again")}})

		if _, ok := lastBody["provider"]; ok {
			t.Errorf("should not pin without encrypted reasoning")
		}
	})

	t.Run("NoPinningUnknownProvider", func(t *testing.T) {
		t.Setenv("OPENROUTER_PIN_PROVIDER", "")
		var lastBody map[string]any
		handler := func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			lastBody = nil
			_ = json.Unmarshal(body, &lastBody)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{
				"id":"x","model":"m",
				"choices":[{"index":0,"message":{
					"role":"assistant","content":"hi",
					"reasoning_details":[{"format":"google-gemini-v1"}]
				}}],
				"provider":"Unknown Provider"
			}`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "key"})

		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi again")}})

		if _, ok := lastBody["provider"]; ok {
			t.Errorf("should not pin unknown provider")
		}
	})

	t.Run("EnvVarPinning", func(t *testing.T) {
		t.Setenv("OPENROUTER_PIN_PROVIDER", "google-vertex")
		var lastBody map[string]any
		handler := func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			lastBody = nil
			_ = json.Unmarshal(body, &lastBody)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "key"})

		// First request should already be pinned
		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
		prov, ok := lastBody["provider"].(map[string]any)
		if !ok {
			t.Fatalf("first request missing provider field with env var set")
		}
		order := prov["order"].([]any)
		if len(order) != 1 || order[0] != "google-vertex" {
			t.Errorf("provider.order = %v, want [google-vertex]", order)
		}
	})

	t.Run("NonOpenRouterNeverPins", func(t *testing.T) {
		t.Setenv("OPENROUTER_PIN_PROVIDER", "google-vertex") // even if set, should be ignored
		var lastBody map[string]any
		handler := func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			lastBody = nil
			_ = json.Unmarshal(body, &lastBody)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{
				"id":"x","model":"m",
				"choices":[{"index":0,"message":{
					"role":"assistant","content":"hi",
					"reasoning_details":[{"format":"google-gemini-v1"}]
				}}],
				"provider":"Google AI Studio"
			}`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		// Name is NOT "openrouter"
		p := New(Config{Name: "google", BaseURL: srv.URL, APIKey: "key"})

		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi again")}})

		if _, ok := lastBody["provider"]; ok {
			t.Errorf("non-openrouter provider should never pin")
		}
	})
}
