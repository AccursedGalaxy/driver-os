package agent

import (
	"fmt"
	"strings"

	"github.com/AccursedGalaxy/driver-os/runspec"
	"github.com/AccursedGalaxy/driver-os/llm"
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
func resolveSystemPrompt(pol runspec.PolicyValue, model llm.Provider) (prompt, note string, err error) {
	switch pol.PromptProfile {
	case "", "legacy":
		prompt, note = nativeSystemPrompt(), ""
	case "structured":
		prompt, note = structuredSystemPrompt(), ""
	case "auto":
		id := modelIDOf(model)
		switch fam := modelFamily(id); fam {
		case famScope:
			prompt, note = structuredSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s (structured)", id, fam)
		case famPersistence:
			prompt, note = persistenceSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s (structured+persistence)", id, fam)
		default:
			prompt, note = nativeSystemPrompt(), fmt.Sprintf("prompt profile auto: model %q → family %s — TERSE FALLBACK (add it to promptFamilies if this is wrong)", id, fam)
		}
	default:
		return "", "", fmt.Errorf("unknown PromptProfile %q (valid: \"\", \"legacy\", \"structured\", \"auto\")", pol.PromptProfile)
	}

	if pol.CodeAct {
		prompt += codeActAddendum
		if note != "" {
			note += "; "
		}
		note += "code-as-action mode ON"
	}
	if pol.ReproFirst {
		prompt += reproFirstAddendum
		if note != "" {
			note += "; "
		}
		note += "repro-first enforcement ON"
	}
	if pol.ReproGate {
		prompt += reproGateAddendum
		if note != "" {
			note += "; "
		}
		note += "repro-first mode ON"
	}
	if pol.BatchReads {
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

const reproGateAddendum = "\n\nTRACE-THE-PRODUCER, THEN REPRODUCE\n" +
	"A passing build or green existing tests are NOT sufficient evidence a bug is fixed — and neither is a reproduction that only exercises the case you first thought of. The real bug usually lives in a code path you have not yet conceived, so a test written from your current mental model will pass while the bug survives. Before your final answer you MUST:\n" +
	"- TRACE THE PRODUCER: for every value your fix relies on being correct, do NOT assume it is already right. Find where that value is PRODUCED (the function and the branch that assigns it) and enumerate EVERY branch or return case of that producer. For each case, check whether the value is correct for the invariant you are enforcing. Bugs of this class are typically a producer returning a wrong value in ONE branch — not the consumer you are tempted to edit.\n" +
	"- FIX AT THE RIGHT LAYER: prefer correcting the producer that returns the wrong value over patching each consumer. A consumer-only fix that assumes the incoming value is already correct will silently miss the branches where it is not.\n" +
	"- REPRODUCE EVERY BRANCH: before editing production code, write a check that FAILS on the unmodified code and that exercises EACH producer branch you enumerated (every distinct input regime — e.g. boundary-straddling, entirely-inside, entirely-outside), not a single happy-path example. Run it and confirm it is RED. If you cannot make it fail, you do not yet understand the bug — keep tracing, do NOT start fixing.\n" +
	"- CONFIRM RED->GREEN: after fixing, re-run your reproduction across all branches, confirm it now PASSES, and confirm you did not break neighboring behavior.\n" +
	"- Only THEN answer, naming which producer(s) you traced, every branch you checked, and why your reproduction covers them. If you skipped any step, say so explicitly and why."

const reproFirstAddendum = "\n\nREPRO-FIRST ENFORCEMENT\n" +
	"Before your fix can be accepted, write a NEW test file that reproduces the bug, then call declare_repro with {path, cmd}. Use a TARGETED command such as `go test -run TestX ./pkg`, not the full suite. The harness will run that command on the pristine base tree with your repro file injected; it must fail as a TEST failure, not a build/setup failure. After validation the repro file is locked. Your final answer is accepted only after the same command passes on the fixed tree. If the task genuinely cannot be reproduced by an executable test, call skip_repro with a non-empty reason and say so in your answer."
