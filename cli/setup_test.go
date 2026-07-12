package cli

import (
	"errors"
	"testing"
)

// D6 provider/model selection. These pin the resolution rules the spec promises:
// the -model flag wins over env wins over default; auto-infer order; and a named
// provider whose key is absent is a clear error, not a silent fallback.

func TestModelOr(t *testing.T) {
	t.Setenv("SOME_MODEL", "from-env")
	if got := modelOr("from-flag", "SOME_MODEL", "fallback"); got != "from-flag" {
		t.Errorf("flag should win, got %q", got)
	}
	if got := modelOr("", "SOME_MODEL", "fallback"); got != "from-env" {
		t.Errorf("env should be used when no flag, got %q", got)
	}
	if got := modelOr("", "UNSET_MODEL_XYZ", "fallback"); got != "fallback" {
		t.Errorf("fallback should be used when neither set, got %q", got)
	}
}

// clearProviderKeys removes every provider key so a test starts from a known
// no-key state; t.Setenv restores them after the test.
func clearProviderKeys(t *testing.T) {
	t.Helper()
	for _, k := range []string{"OPENROUTER_API_KEY", "X_AI_API_KEY", "OPENAI_API_KEY",
		"ANTHROPIC_API_KEY", "CLAUDE_API_KEY",
		"OPENROUTER_MODEL", "XAI_MODEL", "OPENAI_MODEL", "ANTHROPIC_MODEL"} {
		t.Setenv(k, "")
	}
}

func TestPickProvider_AutoInferOrderAndModel(t *testing.T) {
	clearProviderKeys(t)
	t.Setenv("OPENROUTER_API_KEY", "k")
	t.Setenv("X_AI_API_KEY", "k") // both set: OpenRouter wins the auto order.

	p, err := PickProvider("", "")
	if err != nil {
		t.Fatalf("auto with keys errored: %v", err)
	}
	if p.Name() != "openrouter" {
		t.Errorf("auto order should prefer openrouter, got %q", p.Name())
	}
	if mp := p.(interface{ Model() string }); mp.Model() != "openai/gpt-4o-mini" {
		t.Errorf("default model wrong: %q", mp.Model())
	}

	// -model overrides the default even in auto mode.
	p, _ = PickProvider("", "anthropic/claude-x")
	if mp := p.(interface{ Model() string }); mp.Model() != "anthropic/claude-x" {
		t.Errorf("-model should override, got %q", mp.Model())
	}
}

func TestPickProvider_NoKey(t *testing.T) {
	clearProviderKeys(t)
	_, err := PickProvider("", "")
	if !errors.Is(err, ErrNoProviderKey) {
		t.Errorf("auto with no keys should return ErrNoProviderKey, got %v", err)
	}
}

func TestPickProvider_NamedRequiresItsKey(t *testing.T) {
	clearProviderKeys(t)
	// Named provider, key absent → a clear error (not a fallback to another provider).
	if _, err := PickProvider("openai", ""); err == nil {
		t.Error("named openai without OPENAI_API_KEY should error")
	}
	// With its key, it builds — even if a DIFFERENT provider's key is also set.
	t.Setenv("OPENROUTER_API_KEY", "k")
	t.Setenv("OPENAI_API_KEY", "k")
	p, err := PickProvider("openai", "")
	if err != nil {
		t.Fatalf("openai with key errored: %v", err)
	}
	if p.Name() != "openai" {
		t.Errorf("should honor the named provider, got %q", p.Name())
	}
}

func TestPickProvider_OllamaKeyless(t *testing.T) {
	clearProviderKeys(t)
	p, err := PickProvider("ollama", "llama3.2")
	if err != nil {
		t.Fatalf("ollama should not need a key: %v", err)
	}
	if p.Name() != "ollama" {
		t.Errorf("got %q", p.Name())
	}
}

func TestPickProvider_Unknown(t *testing.T) {
	clearProviderKeys(t)
	if _, err := PickProvider("anthropic", ""); err == nil {
		t.Error("unknown provider should error")
	}
}

// SplitModelRef must split only KNOWN provider prefixes — OpenRouter ids
// legitimately contain ":" (":free" variants), and those must pass through.
func TestSplitModelRef(t *testing.T) {
	cases := []struct{ ref, provider, model string }{
		{"anthropic:claude-fable-5", "anthropic", "claude-fable-5"},
		{"openrouter:openai/gpt-5.5", "openrouter", "openai/gpt-5.5"},
		{"openai/gpt-5.5", "", "openai/gpt-5.5"},
		{"deepseek/deepseek-chat:free", "", "deepseek/deepseek-chat:free"},
		{"ollama:llama3.2", "ollama", "llama3.2"}, {"mock:/tmp/demo.json", "mock", "/tmp/demo.json"},
		{"", "", ""},
		{":claude", "", ":claude"},
	}
	for _, c := range cases {
		p, m := SplitModelRef(c.ref)
		if p != c.provider || m != c.model {
			t.Errorf("SplitModelRef(%q) = (%q, %q), want (%q, %q)", c.ref, p, m, c.provider, c.model)
		}
	}
}
