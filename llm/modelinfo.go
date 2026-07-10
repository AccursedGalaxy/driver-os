package llm

import "strings"

// ModelInfo describes model limits that are useful to callers before a request
// is sent. A zero field means that limit is unknown; callers must treat an
// unknown limit as having no proactive limit.
type ModelInfo struct {
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

// Lookup returns cataloged metadata for model. Matching is case-sensitive and
// accepts either a bare model slug or the same slug prefixed by its provider
// (for example, "gpt-5.6-luna" and "openai/gpt-5.6-luna"). Unknown models
// return the zero ModelInfo; the catalog deliberately does not guess.
func Lookup(model string) ModelInfo {
	if strings.HasPrefix(model, "openai/") {
		model = strings.TrimPrefix(model, "openai/")
		if strings.HasPrefix(model, "gpt-5.6-") && len(model) > len("gpt-5.6-") {
			return ModelInfo{ContextWindow: 1_050_000}
		}
		return ModelInfo{}
	}
	if strings.HasPrefix(model, "anthropic/") {
		model = strings.TrimPrefix(model, "anthropic/")
		if model == "claude-fable-5" || model == "claude-opus-4.8" {
			return ModelInfo{ContextWindow: 200_000}
		}
		return ModelInfo{}
	}
	if strings.HasPrefix(model, "gpt-5.6-") && len(model) > len("gpt-5.6-") {
		return ModelInfo{ContextWindow: 1_050_000}
	}
	switch model {
	case "claude-fable-5", "claude-opus-4.8":
		return ModelInfo{ContextWindow: 200_000}
	default:
		return ModelInfo{}
	}
}
