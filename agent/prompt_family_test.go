package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// PROMPT-SKILLS slice 3: the family matcher is table-driven and tested against
// the whole model catalog (council O5/O9). A PRIMARY cheap target routing to
// the terse fallback is a test FAILURE, not a logged surprise.

func TestModelFamilyCatalog(t *testing.T) {
	cases := []struct{ id, want string }{
		// Primary cheap targets (docs/specs/PROMPT-SKILLS.md Goal) — MUST hit
		// persistence explicitly, never the fallback.
		{"google/gemini-3-flash-preview", famPersistence},
		{"deepseek/deepseek-v4-flash", famPersistence},
		{"deepseek/deepseek-v4-pro", famPersistence},
		{"z-ai/glm-5", famPersistence},
		// The rest of the working catalog (CLAUDE.md table + roster).
		{"openai/gpt-5.5", famPersistence},
		{"google/gemini-3.1-pro", famPersistence},
		{"google/gemini-2.5-flash-lite", famPersistence},
		{"anthropic/claude-opus-4.8", famScope},
		{"anthropic:claude-fable-5", famScope}, // driver 'provider:model' syntax.
		{"claude-haiku-4-5-20251001", famScope},
		{"qwen/qwen3-coder", famPersistence},
		{"moonshotai/kimi-k2", famPersistence},
		{"x-ai/grok-4", famPersistence},
		{"mistralai/mistral-large", famPersistence},
		// Adversarial ids: OpenRouter decorations, case, whitespace.
		{"deepseek/deepseek-v4-flash:free", famPersistence},
		{"z-ai/glm-5:nitro", famPersistence},
		{"Google/Gemini-3-Flash-Preview", famPersistence},
		{"  openai/gpt-5.5  ", famPersistence},
		// Unknown vendors and the empty id route to the fallback.
		{"acme/frontier-9000", famUnknown},
		{"", famUnknown},
	}
	for _, c := range cases {
		if got := modelFamily(c.id); got != c.want {
			t.Errorf("modelFamily(%q) = %s, want %s", c.id, got, c.want)
		}
	}
}

// modelStub lets the tests hand resolveSystemPrompt a provider with a chosen
// id without a real backend.
type modelStub struct {
	nativeScript
	id string
}

func (m *modelStub) Model() string { return m.id }

func TestResolveSystemPromptAuto(t *testing.T) {
	resolve := func(id string) (string, string) {
		t.Helper()
		p, note, err := resolveSystemPrompt(Config{PromptProfile: "auto", Model: &modelStub{id: id}})
		if err != nil {
			t.Fatalf("auto(%q): %v", id, err)
		}
		return p, note
	}

	// Persistence family: structured base + anti-quitting block, routing noted.
	p, note := resolve("deepseek/deepseek-v4-flash")
	if !strings.Contains(p, "Working rules:") || !strings.Contains(p, "Persistence:") {
		t.Errorf("persistence variant missing base or persistence block")
	}
	if !strings.Contains(note, "persistence") || !strings.Contains(note, "deepseek/deepseek-v4-flash") {
		t.Errorf("routing note doesn't name family+model: %q", note)
	}

	// Scope family: structured base WITHOUT the push (Claude needs restraint).
	p, note = resolve("anthropic/claude-opus-4.8")
	if !strings.Contains(p, "Working rules:") || strings.Contains(p, "Persistence:") {
		t.Errorf("scope variant should be structured without the persistence block")
	}
	if !strings.Contains(note, famScope) {
		t.Errorf("routing note doesn't name the scope family: %q", note)
	}

	// Unknown model: terse legacy fallback, LOUD note.
	p, note = resolve("acme/frontier-9000")
	if p != nativeSystemPrompt() {
		t.Errorf("unknown model should get the terse legacy prompt")
	}
	if !strings.Contains(note, "TERSE FALLBACK") {
		t.Errorf("fallback routing must be loud, got: %q", note)
	}

	// A provider that exposes no Model() routes to the fallback, not a crash.
	p, note, err := resolveSystemPrompt(Config{PromptProfile: "auto", Model: &nativeScript{}})
	if err != nil || p != nativeSystemPrompt() || !strings.Contains(note, "TERSE FALLBACK") {
		t.Errorf("no-Model() provider: p-legacy=%v note=%q err=%v", p == nativeSystemPrompt(), note, err)
	}
}

// The routing note must reach the observer on a real native run.
type noteRecorder struct {
	nopObserver
	notes []string
}

func (n *noteRecorder) Note(s string) { n.notes = append(n.notes, s) }

func TestRunNativeAutoProfileNotesRouting(t *testing.T) {
	ns := &modelStub{nativeScript: nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}, id: "z-ai/glm-5"}
	rec := &noteRecorder{}
	_, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, nil), Task: "t", PromptProfile: "auto", Obs: rec})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	found := false
	for _, n := range rec.notes {
		if strings.Contains(n, "prompt profile auto") && strings.Contains(n, "glm-5") {
			found = true
		}
	}
	if !found {
		t.Errorf("routing note not surfaced on the observer: %v", rec.notes)
	}
	if len(ns.calls) == 0 || !strings.Contains(ns.calls[0].System, "Persistence:") {
		t.Errorf("glm-5 request does not carry the persistence variant")
	}
}
