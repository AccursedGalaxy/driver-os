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
			// Override map: display names whose real slug differs from the
			// lowercase+hyphenate algorithmic default.
			{"Z.AI", "z-ai"},
			{"Moonshot AI", "moonshotai"},
			{"Mancer 2", "mancer"},
			{"InferenceNet", "inference-net"},
			{"AionLabs", "aion-labs"},
			{"FakeProvider", "fake-provider"},
			{"AtlasCloud", "atlas-cloud"},
			{"Google", "google-vertex"},
			{"OpenInference", "open-inference"},
			{"Sakana AI", "sakana"},
			{"Google AI Studio", "google-ai-studio"},
			// Algorithmic default: lowercase + spaces→hyphens.
			{"OpenAI", "openai"},
			{"DeepInfra", "deepinfra"},
			{"DeepSeek", "deepseek"},
			{"", ""},
		}
		for _, tt := range tests {
			if got := pinProviderName(tt.display); got != tt.want {
				t.Errorf("pinProviderName(%q) = %q, want %q", tt.display, got, tt.want)
			}
		}
	})

	// maybePin unit tests — test the function directly via the unexported
	// fields on the same package.

	t.Run("maybePinSetsOnFirstCall", func(t *testing.T) {
		p := &Provider{isOpenRouter: true}
		p.maybePin(json.RawMessage(`"Google AI Studio"`))
		if p.pinnedProvider != "google-ai-studio" {
			t.Errorf("pinnedProvider = %q, want google-ai-studio", p.pinnedProvider)
		}
	})

	t.Run("maybePinSecondCallIsNoOp", func(t *testing.T) {
		p := &Provider{isOpenRouter: true}
		// First call pins to google-ai-studio.
		p.maybePin(json.RawMessage(`"Google AI Studio"`))
		// Second call with a different provider must NOT overwrite.
		p.maybePin(json.RawMessage(`"Google"`))
		if p.pinnedProvider != "google-ai-studio" {
			t.Errorf("pinnedProvider = %q, want google-ai-studio (once-only pin)", p.pinnedProvider)
		}
	})

	t.Run("maybePinNonOpenRouterNoOp", func(t *testing.T) {
		p := &Provider{isOpenRouter: false}
		p.maybePin(json.RawMessage(`"Google AI Studio"`))
		if p.pinnedProvider != "" {
			t.Errorf("pinnedProvider = %q, want empty (non-openrouter)", p.pinnedProvider)
		}
	})

	t.Run("maybePinRawNameFallback", func(t *testing.T) {
		// pinProviderName now maps "DeepInfra" via the algorithmic
		// default (lowercase + spaces→hyphens), producing "deepinfra".
		p := &Provider{isOpenRouter: true}
		p.maybePin(json.RawMessage(`"DeepInfra"`))
		if p.pinnedProvider != "deepinfra" {
			t.Errorf("pinnedProvider = %q, want deepinfra", p.pinnedProvider)
		}
	})

	t.Run("maybePinEmptyProviderNoOp", func(t *testing.T) {
		p := &Provider{isOpenRouter: true}
		p.maybePin(json.RawMessage(`""`))
		if p.pinnedProvider != "" {
			t.Errorf("pinnedProvider = %q, want empty (empty name)", p.pinnedProvider)
		}
	})

	t.Run("maybePinWithoutEncryptedReasoning", func(t *testing.T) {
		// The key new behavior: pinning happens even when there is NO
		// encrypted reasoning. Before the fix, this would stay empty.
		p := &Provider{isOpenRouter: true}
		p.maybePin(json.RawMessage(`"Google AI Studio"`))
		if p.pinnedProvider == "" {
			t.Error("pinnedProvider is empty; should pin even without encrypted reasoning")
		}
	})

	// End-to-end tests via Generate.

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

	t.Run("PinningWithoutEncryptedReasoning", func(t *testing.T) {
		// After the generalization, pinning happens for EVERY model, not
		// just Gemini reasoning runs. A response with a provider field but
		// NO reasoning_details must still trigger pinning.
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

		// First request: no pinning yet.
		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})

		// Second request: must now be pinned (this is the new behavior).
		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi again")}})

		prov, ok := lastBody["provider"].(map[string]any)
		if !ok {
			t.Fatalf("second request should have provider field after generalisation: %v", lastBody)
		}
		order := prov["order"].([]any)
		if len(order) != 1 || order[0] != "google-ai-studio" {
			t.Errorf("provider.order = %v, want [google-ai-studio]", order)
		}
	})

	t.Run("PinningRawNameFallback", func(t *testing.T) {
		// pinProviderName now maps "DeepInfra" via the algorithmic
		// default (lowercase + spaces→hyphens) to "deepinfra".
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
				"provider":"DeepInfra"
			}`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "key"})

		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}})
		p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi again")}})

		prov, ok := lastBody["provider"].(map[string]any)
		if !ok {
			t.Fatalf("second request should have provider field: %v", lastBody)
		}
		order := prov["order"].([]any)
		if len(order) != 1 || order[0] != "deepinfra" {
			t.Errorf("provider.order = %v, want [deepinfra]", order)
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

	t.Run("PreferredUpstreamSteering", func(t *testing.T) {
		// openai/* on OpenRouter: every request carries standing
		// order-steering toward the cache-serving OpenAI upstream with
		// fallbacks allowed, and a response served by another upstream
		// (Azure) must NOT pin — the next request steers back to OpenAI.
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
				"provider":"Azure"
			}`)
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "key", Model: "openai/gpt-5.6-sol"})

		check := func(reqNo int) {
			prov, ok := lastBody["provider"].(map[string]any)
			if !ok {
				t.Fatalf("request %d missing provider steering field: %v", reqNo, lastBody)
			}
			order := prov["order"].([]any)
			if len(order) != 1 || order[0] != "openai" {
				t.Errorf("request %d provider.order = %v, want [openai]", reqNo, order)
			}
			if prov["allow_fallbacks"] != true {
				t.Errorf("request %d allow_fallbacks = %v, want true", reqNo, prov["allow_fallbacks"])
			}
		}

		if _, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}}); err != nil {
			t.Fatal(err)
		}
		check(1)
		// The Azure-served response above must not have pinned.
		if p.pinnedProvider != "" {
			t.Errorf("pinnedProvider = %q, want empty (steered models never response-pin)", p.pinnedProvider)
		}
		if _, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi again")}}); err != nil {
			t.Fatal(err)
		}
		check(2)
	})

	t.Run("EnvPinOverridesPreferred", func(t *testing.T) {
		// OPENROUTER_PIN_PROVIDER stays authoritative over the steering
		// default: pinned from turn 1, allow_fallbacks false.
		t.Setenv("OPENROUTER_PIN_PROVIDER", "azure")
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

		p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "key", Model: "openai/gpt-5.6-sol"})
		if _, err := p.Generate(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}}); err != nil {
			t.Fatal(err)
		}
		prov, ok := lastBody["provider"].(map[string]any)
		if !ok {
			t.Fatalf("missing provider field with env pin set")
		}
		order := prov["order"].([]any)
		if len(order) != 1 || order[0] != "azure" {
			t.Errorf("provider.order = %v, want [azure] (env pin wins)", order)
		}
		if prov["allow_fallbacks"] != false {
			t.Errorf("allow_fallbacks = %v, want false (env pin is a pin)", prov["allow_fallbacks"])
		}
	})

	t.Run("preferredUpstreams", func(t *testing.T) {
		tests := []struct {
			model string
			want  int // len of preferred list
		}{
			{"openai/gpt-5.6-sol", 1},
			{"openai/gpt-5.5", 1},
			{"deepseek/deepseek-v4-flash", 0},
			{"google/gemini-3-flash", 0},
			{"anthropic/claude-opus-4.8", 0},
			{"", 0},
		}
		for _, tt := range tests {
			if got := preferredUpstreams(tt.model); len(got) != tt.want {
				t.Errorf("preferredUpstreams(%q) = %v, want len %d", tt.model, got, tt.want)
			}
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
