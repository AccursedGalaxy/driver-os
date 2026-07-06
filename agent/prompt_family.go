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
		prompt, note = nativeSystemPrompt(), ""
	case "structured":
		prompt, note = structuredSystemPrompt(), ""
	case "auto":
		id := modelIDOf(cfg.Model)
		switch fam := modelFamily(id); fam {
		case famScope:
			prompt, note = structuredSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s (structured)", id, fam)
		case famPersistence:
			prompt, note = persistenceSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s (structured+persistence)", id, fam)
		default:
			prompt, note = nativeSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s — TERSE FALLBACK (add it to promptFamilies if this is wrong)", id, fam)
		}
	default:
		return "", "", fmt.Errorf("unknown PromptProfile %q (valid: \"\", \"legacy\", \"structured\", \"auto\")", cfg.PromptProfile)
	}

	if cfg.CodeAct {
		prompt += codeActAddendum
		if note != "" {
			note += "; "
		}
		note += "code-as-action mode ON"
	}
	if cfg.BatchReads {
		prompt += batchReadsAddendum
		if note != "" {
			note += "; "
		}
		note += "batch-reads mode ON"
	}
	return prompt, note, nil
}

const batchReadsAddendum = "\n\nBATCH INDEPENDENT READS\nWhen you need information from several files, symbols, or searches whose results do NOT depend on each other, request them together in a SINGLE turn — emit multiple read_file / go_doc / search / list_dir tool calls at once rather than one per turn. The harness fetches parallel-safe reads concurrently, so batching cuts round-trips at no extra cost. Only serialize a read when it genuinely depends on an earlier result (e.g. a path you must first discover with list_dir, or a symbol you learned from a prior file). Never batch write_file, edit_file, or run — only read-only calls."

const codeActAddendum = "\n\nCODE-AS-ACTION MODE\nYour primary action is executable shell via the `run` tool. Prefer to accomplish work by composing and running code rather than issuing many discrete file operations:\n- Locate and inspect with shell (rg/grep, sed -n, cat) instead of guessing.\n- Apply changes as code where practical — a targeted `sed -i`, a small script, or a heredoc — and combine build+test into one command (e.g. `go test ./... 2>&1 | tail -40`), so each `run` both changes state and verifies it.\n- Batch related steps into a single `run` with `&&` or a heredoc script to cut round-trips.\n- Reach for the dedicated read_file/write_file/edit_file tools only when a shell command would be clearly more error-prone (e.g. a delicate multi-line edit in a large file).\nThink in terms of \"what program produces this change and proves it\", not \"what single tool call comes next\"."
