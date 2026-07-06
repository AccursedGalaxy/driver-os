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

// TestResolveSystemPromptCodeAct: the -codeact knob appends the code-as-action
// block onto whatever base profile resolved, notes it, and is byte-identical to
// off when false — arms differ in this flag alone (docs/specs/CODEACT-SCREEN.md).
func TestResolveSystemPromptCodeAct(t *testing.T) {
	off, _, err := resolveSystemPrompt(Config{CodeAct: false})
	if err != nil {
		t.Fatalf("codeact off: %v", err)
	}
	if strings.Contains(off, "CODE-AS-ACTION MODE") {
		t.Errorf("base prompt must NOT contain the addendum when CodeAct is off")
	}

	on, note, err := resolveSystemPrompt(Config{CodeAct: true})
	if err != nil {
		t.Fatalf("codeact on: %v", err)
	}
	if !strings.Contains(on, "CODE-AS-ACTION MODE") {
		t.Errorf("prompt must contain the addendum when CodeAct is on")
	}
	// On == base (off) + addendum: the flag only ADDS, never rewrites the base.
	if !strings.HasPrefix(on, off) {
		t.Errorf("codeact prompt must be the base prompt plus the addendum, not a rewrite")
	}
	if !strings.Contains(note, "code-as-action mode ON") {
		t.Errorf("note must announce code-as-action mode, got %q", note)
	}

	// Composes with a non-default profile and still carries both parts.
	structured, _, err := resolveSystemPrompt(Config{PromptProfile: "structured", CodeAct: true})
	if err != nil {
		t.Fatalf("structured+codeact: %v", err)
	}
	if !strings.Contains(structured, "Working rules:") || !strings.Contains(structured, "CODE-AS-ACTION MODE") {
		t.Errorf("structured+codeact must contain both the structured base and the addendum")
	}

	// The error path (unknown profile) is unaffected by CodeAct.
	if _, _, err := resolveSystemPrompt(Config{PromptProfile: "bogus", CodeAct: true}); err == nil {
		t.Errorf("unknown profile must still error even with CodeAct on")
	}
}

// TestResolveSystemPromptBatchReads: the -batch-reads knob appends the
// batch-reads block onto whatever base profile resolved, notes it, and is
// byte-identical to off when false — arms differ in this flag alone.
func TestResolveSystemPromptBatchReads(t *testing.T) {
	off, _, err := resolveSystemPrompt(Config{BatchReads: false})
	if err != nil {
		t.Fatalf("batch-reads off: %v", err)
	}
	if strings.Contains(off, "BATCH INDEPENDENT READS") {
		t.Errorf("base prompt must NOT contain the addendum when BatchReads is off")
	}

	on, note, err := resolveSystemPrompt(Config{BatchReads: true})
	if err != nil {
		t.Fatalf("batch-reads on: %v", err)
	}
	if !strings.Contains(on, "BATCH INDEPENDENT READS") {
		t.Errorf("prompt must contain the addendum when BatchReads is on")
	}
	if !strings.Contains(note, "batch-reads mode ON") {
		t.Errorf("note must mention batch-reads: %q", note)
	}

	// Composes with other profiles.
	structured, _, _ := resolveSystemPrompt(Config{PromptProfile: "structured", BatchReads: true})
	if !strings.Contains(structured, "Working rules:") || !strings.Contains(structured, "BATCH INDEPENDENT READS") {
		t.Errorf("batch-reads should compose with structured profile")
	}

	// Unknown profile still errors.
	_, _, err = resolveSystemPrompt(Config{PromptProfile: "invalid", BatchReads: true})
	if err == nil {
		t.Errorf("unknown profile should still error with BatchReads on")
	}
}
