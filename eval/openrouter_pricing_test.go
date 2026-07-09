package eval

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestMain(m *testing.M) {
	openRouterModelsURL = ""
	os.Exit(m.Run())
}

func TestOpenRouterPriceFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor/new-model","pricing":{"prompt":"0.000002","completion":"0.00001","input_cache_read":"0.0000002"}}]}`))
	}))
	defer server.Close()
	resetOpenRouterPricing(server.URL)

	u := llm.Usage{PromptTokens: 100_000, CompletionTokens: 50_000, CachedTokens: 20_000}
	got, ok := CostOf("vendor/new-model", u)
	want := Price{InPerM: 2, OutPerM: 10, CacheReadPerM: .2}.Cost(u)
	if !ok || math.Abs(got-want) > 1e-12 {
		t.Fatalf("fallback cost=%v ok=%v, want %v and true", got, ok, want)
	}
}

func TestOpenRouterStaticPriceWins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-5.5","pricing":{"prompt":"1","completion":"1"}}]}`))
	}))
	defer server.Close()
	resetOpenRouterPricing(server.URL)

	u := llm.Usage{PromptTokens: 100_000, CompletionTokens: 50_000}
	got, ok := CostOf("openai/gpt-5.5", u)
	want := Pricing["openai/gpt-5.5"].Cost(u)
	if !ok || got != want {
		t.Fatalf("static cost=%v ok=%v, want %v and true", got, ok, want)
	}
}

func TestOpenRouterPriceFallbackUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	url := server.URL
	server.Close()
	resetOpenRouterPricing(url)

	if got, ok := CostOf("vendor/missing-model", llm.Usage{PromptTokens: 1}); ok || got != 0 {
		t.Fatalf("unavailable fallback=%v ok=%v, want 0 and false", got, ok)
	}
}

func TestOpenRouterPriceFallbackFetchesOnce(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor/once-model","pricing":{"prompt":"0.000001","completion":"0.000001"}}]}`))
	}))
	defer server.Close()
	resetOpenRouterPricing(server.URL)

	for i := 0; i < 4; i++ {
		CostOf("vendor/once-model", llm.Usage{PromptTokens: 1})
		CostOf("vendor/other-model", llm.Usage{PromptTokens: 1})
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
}
