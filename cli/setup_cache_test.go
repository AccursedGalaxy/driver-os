package cli

import (
	"testing"

	"github.com/AccursedGalaxy/driver-os/provider/openaicompat"
)

func TestPickProvider_OpenRouterCache(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")

	p, err := PickProvider("openrouter", "anthropic/claude-opus-4.8")
	if err != nil {
		t.Fatalf("PickProvider failed: %v", err)
	}

	op, ok := p.(*openaicompat.Provider)
	if !ok {
		t.Fatalf("expected *openaicompat.Provider, got %T", p)
	}

	if !op.PromptCacheEnabled() {
		t.Error("expected PromptCacheEnabled() to be true for OpenRouter")
	}
}
