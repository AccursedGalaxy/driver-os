package agent

import (
	"fmt"
	"strings"
)

// PROMPT-SKILLS slice 3: per-model-family prompt variants (the opencode
// pattern). PromptProfile "auto" routes by model FAMILY: models that need
// anti-quitting pressure (GPT-class, DeepSeek, GLM, Gemini — the harness's
// primary cheap targets) get the persistence variant; Claude-class models get
// scope discipline instead (the structured prompt already is that); an UNKNOWN
// model gets the terse legacy prompt, and the routing decision is Note'd on
// the observer either way, so a misroute is visible in every transcript —
// never silent (council O5/O9).

const (
	famScope       = "scope"       // Claude-class: over-eager, needs restraint not push.
	famPersistence = "persistence" // GPT/DeepSeek/GLM/Gemini-class: quits early, needs push.
	famUnknown     = "unknown"     // not in the table: terse fallback, loudly logged.
)

// promptFamilies is the ONE routing table (council O5: centralized,
// table-driven, tested against the whole catalog). Matching is by substring
// over the lowercased full model id, which absorbs provider prefixes
// ("openai/", "anthropic:"), version suffixes, preview tags, and OpenRouter
// decorations (":free", ":nitro") without fragile stripping. First match wins;
// "claude" is first so an id naming both routes to scope.
var promptFamilies = []struct{ substr, family string }{
	{"claude", famScope},
	{"gpt-", famPersistence},
	{"deepseek", famPersistence},
	{"glm", famPersistence},
	{"gemini", famPersistence},
	{"qwen", famPersistence},
	{"kimi", famPersistence},
	{"grok", famPersistence},
	{"mistral", famPersistence},
}

// modelFamily maps a model id to its prompt family. Empty id (a provider that
// doesn't expose one) is unknown, not an error — the fallback covers it.
func modelFamily(id string) string {
	norm := strings.ToLower(strings.TrimSpace(id))
	if norm == "" {
		return famUnknown
	}
	for _, e := range promptFamilies {
		if strings.Contains(norm, e.substr) {
			return e.family
		}
	}
	return famUnknown
}

// persistenceSystemPrompt is the structured prompt plus the anti-quitting
// block. GPT-class/DeepSeek-class agents stop at analysis or the first error
// unless pushed (every serious GPT harness ships this pressure; Claude needs
// the opposite, which is why it is a VARIANT and not part of the base).
func persistenceSystemPrompt() string {
	return structuredSystemPrompt() + `

Persistence: you are fully capable of completing this task with the tools provided, without user help. Keep going until the TASK is solved and verified — do not stop at analysis, a plan, a partial fix, or the first error. Only finish when the work is done and checked, or you have confirmed it is genuinely impossible with the tools you have.`
}

// modelIDOf extracts the configured model id when the provider exposes one
// (openaicompat and anthropic both do). Optional-interface type assertion,
// same seam as DeltaObserver: providers that don't implement it simply route
// to the unknown family.
func modelIDOf(p any) string {
	if m, ok := p.(interface{ Model() string }); ok {
		return m.Model()
	}
	return ""
}

// resolveSystemPrompt maps Config.PromptProfile to the native loop's base
// prompt. The fixed profiles ("", "legacy", "structured") resolve statically;
// "auto" routes by model family and returns a routing note the caller MUST
// surface on the observer (the misroute-visibility requirement). Unknown
// profiles are an ERROR, not a fallback: this knob exists to be A/B'd, and a
// typo that silently ran the wrong arm would corrupt a paid experiment.
func resolveSystemPrompt(cfg Config) (prompt, note string, err error) {
	switch cfg.PromptProfile {
	case "", "legacy":
		return nativeSystemPrompt(), "", nil
	case "structured":
		return structuredSystemPrompt(), "", nil
	case "auto":
		id := modelIDOf(cfg.Model)
		switch fam := modelFamily(id); fam {
		case famScope:
			return structuredSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s (structured)", id, fam), nil
		case famPersistence:
			return persistenceSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s (structured+persistence)", id, fam), nil
		default:
			return nativeSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s — TERSE FALLBACK (add it to promptFamilies if this is wrong)", id, fam), nil
		}
	default:
		return "", "", fmt.Errorf("unknown PromptProfile %q (valid: \"\", \"legacy\", \"structured\", \"auto\")", cfg.PromptProfile)
	}
}
