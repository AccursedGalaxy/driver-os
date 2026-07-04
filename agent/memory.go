// Long-term memory for the agent — the natural extension of Principle 1 (state
// lives in YOUR code) and Principle 3 (context is the only state, so managing it
// IS the engineering). The in-loop `messages` slice is state for ONE run; mneme
// is state that survives ACROSS runs. We recall relevant facts before thinking
// and store what we concluded after answering. The model never drives this — the
// harness decides what to remember and what to surface (Principle 7).
//
// mneme is a standalone library (github.com/AccursedGalaxy/mneme): we feed it
// messages, it extracts durable facts, dedups, and lets us search them back.
package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AccursedGalaxy/mneme"
	mnemeopenai "github.com/AccursedGalaxy/mneme/provider/openai"
	"github.com/AccursedGalaxy/mneme/store/sqlite"
)

// MemoryDBPath is where facts persist between runs. A plain file (pure-Go
// SQLite, no cgo) — delete it to reset the agent's long-term memory.
const MemoryDBPath = ".agent-memory.db"

// memoryTimeout bounds each mneme call (a recall search, or a store extraction).
// The agent loop has iteration caps but memory talks to an embeddings/LLM
// endpoint; a hung endpoint must not hang the run (Principle 5 — termination is
// our job). On timeout the call fails soft: recall returns nothing, store skips.
const memoryTimeout = 30 * time.Second

// agentScope namespaces this agent's memories. mneme isolates facts per scope on
// both write and search, so multiple agents/users can share a store without
// bleeding into each other.
var agentScope = mneme.Scope{AgentID: "driver-os-agent"}

// SetupMemory wires mneme to the SAME OpenRouter key the agent already uses, so
// no extra configuration is needed. Memory is best-effort: with no key (or if
// the store won't open) it returns nil and the agent simply runs without
// cross-session recall — a missing memory is never a crash (Principle 6).
func SetupMemory() (mneme.Memory, error) { return SetupMemoryAt(MemoryDBPath) }

// SetupMemoryAt is SetupMemory with a caller-chosen store path, so multiple
// agents can share ONE database (distinguished by MemoryScope) located wherever
// the caller wants — e.g. a duet workspace, instead of the cwd default. Same
// fail-soft contract: no key returns (nil, nil) and the agent runs statelessly.
func SetupMemoryAt(dbPath string) (mneme.Memory, error) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return nil, nil // no key -> run statelessly, not an error.
	}
	const base = "https://openrouter.ai/api/v1"

	st, err := sqlite.Open(dbPath)
	if err != nil {
		return nil, err
	}
	mem, err := mneme.New(
		mneme.WithStore(st),
		// Consolidate (not the default Additive) is what lets memory STAY TRUE over
		// time. Under Additive, Add only ever appends: bump the Go version and the
		// store holds BOTH the old and new value forever, recalling stale facts
		// beside fresh ones. Consolidate runs a second LLM call per Add that decides,
		// per existing fact, whether the new ones ADD / UPDATE / DELETE / leave it —
		// so a changed fact REPLACES the stale one instead of piling up. The cost is
		// one extra LLM call (+ a cheap embed/search) per Add, and only when there are
		// existing facts in scope to reconcile against (the first fact in a scope still
		// costs nothing extra). This is the right trade for a repo-facts agent, where
		// the truth mutates.
		//
		// Crucially, mneme retrieves the facts-to-reconcile by the EXTRACTED CANDIDATES,
		// not by the conversation: storing "Go 1.24" pulls up the stored "Go 1.23" to
		// overturn it even when 1.23 is unlike the rest of the turn. That candidate-keyed
		// window (DefaultConsolidationTopK=30, left at the default) is what makes our
		// "a later grounded run corrects the stale fact" claim actually hold.
		mneme.WithStrategy(mneme.Consolidate),
		mneme.WithLLM(&mnemeopenai.LLM{
			BaseURL: base, APIKey: key,
			Model: envOr("MNEME_LLM_MODEL", "openai/gpt-4o-mini"),
		}),
		// WARNING: the embedding model is pinned to the store. Stored fact
		// vectors and query vectors must come from the SAME model — they live in
		// the same vector space. mneme records the embedder's model name (our
		// Embedder implements Name()) on first insert and now FAILS LOUDLY:
		// changing MNEME_EMBED_MODEL after facts exist makes New return an
		// *EmbedderMismatchError instead of silently comparing across spaces, so
		// SetupMemory surfaces it and we run without memory rather than on
		// garbage recall. If you change it intentionally, delete
		// .agent-memory.db first (or pass mneme.AllowEmbedderMismatch()).
		mneme.WithEmbedder(&mnemeopenai.Embedder{
			BaseURL: base, APIKey: key,
			Model: envOr("MNEME_EMBED_MODEL", "text-embedding-3-small"),
		}),
	)
	if err != nil {
		// New can error: it returns an *EmbedderMismatchError when the configured
		// embedder doesn't match what this store was first written with (a changed
		// MNEME_EMBED_MODEL against existing facts). Don't leak the open store
		// handle on that path — the caller fails soft and runs without memory.
		st.Close()
		return nil, err
	}
	return mem, nil
}

// recall fetches facts relevant to the task from past runs and renders them as a
// system-prompt block. Returns "" when there is nothing to add (first run, no
// memory, or a recall error — all non-fatal). The block is explicitly labelled
// as possibly-stale so the model still verifies with tools (Principle 4: observe
// REAL state, don't trust prior text). Consolidate-on-write (see SetupMemory)
// corrects facts that a LATER grounded run re-observes, but a fact no run has
// revisited can still be stale — so the verify-with-tools framing stays.
func recall(ctx context.Context, obs Observer, mem mneme.Memory, scope mneme.Scope, task string) string {
	if mem == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, memoryTimeout)
	defer cancel()
	hits, err := mem.Search(ctx, task, scope, 5)
	if err != nil {
		obs.Note(fmt.Sprintf("memory: recall failed (non-fatal): %v", err))
		return ""
	}
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nMEMORY — facts you learned in PAST runs (may be stale; verify with tools before relying on them):\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "  - %s\n", h.Text)
	}
	// Status flows through the Observer, never fmt to stdout: stdout is the
	// machine-readable data channel (-format=json/ndjson) and the package contract
	// is "Run never calls fmt itself" (observer.go). See harness review finding #2.
	obs.Note(fmt.Sprintf("memory: recalled %d fact(s) relevant to the task", len(hits)))
	return b.String()
}

// remember stores what the agent concluded so a future run can recall it. We
// hand mneme the task and the final answer; it extracts the durable facts. Best
// effort — a storage failure is logged, never fatal (Principle 6).
//
// The store runs DETACHED from the caller's cancellation (review #4): by the
// time remember is called the answer is already verified and delivered, so a
// Ctrl-C at that instant must not throw the fact away — the whole point of the
// grounded gate is that these facts are expensive to re-earn. memoryTimeout is
// the only bound (a hung endpoint must still not hang us, Principle 5).
//
// The loops call this through rememberAsync so the RunResult is returned (and
// the CLI emits it) without waiting the ≤memoryTimeout extraction+embedding
// round-trip; a process-exiting caller awaits via RunResult.AwaitMemory.
func remember(ctx context.Context, obs Observer, mem mneme.Memory, scope mneme.Scope, task, answer string) {
	if mem == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memoryTimeout)
	defer cancel()
	written, err := mem.Add(ctx, []mneme.Message{
		{Role: "user", Content: task},
		{Role: "assistant", Content: answer},
	}, scope)
	if err != nil {
		obs.Note(fmt.Sprintf("memory: store failed (non-fatal): %v", err))
		return
	}
	if len(written) > 0 {
		// Through the Observer, not stdout — see recall and finding #2.
		obs.Note(fmt.Sprintf("memory: stored %d new fact(s) for future runs", len(written)))
	}
}

// rememberAsync runs remember in the background and returns a channel that
// closes when the store completes — the handle RunResult.AwaitMemory blocks on.
// nil memory returns nil (nothing to await). This is what lets the loop return
// the result the moment the answer is accepted instead of blocking the caller's
// emission on an LLM round-trip (review #4).
func rememberAsync(ctx context.Context, obs Observer, mem mneme.Memory, scope mneme.Scope, task, answer string) <-chan struct{} {
	if mem == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		remember(ctx, obs, mem, scope, task, answer)
	}()
	return done
}

// StoreMemoryAsync lets an orchestrator that deliberately suppressed in-run
// storage (for example, ladder loser isolation) store the single accepted winner
// without disabling recall for the attempts. It has the same best-effort,
// detached semantics as the loop's own post-answer store.
func StoreMemoryAsync(ctx context.Context, obs Observer, mem mneme.Memory, scope mneme.Scope, task, answer string) <-chan struct{} {
	return rememberAsync(ctx, obs, mem, scopeOrDefault(scope), task, answer)
}

// scopeOrDefault resolves the memory namespace for a run: an explicit
// Config.MemoryScope when set, else the package default. This is what lets two
// agents share one store without their facts bleeding together (each passes its
// own scope, e.g. {AgentID: "adam"} vs {AgentID: "alex"}); a lone CLI run that
// sets nothing keeps the historical single-scope behavior.
func scopeOrDefault(s mneme.Scope) mneme.Scope {
	if s == (mneme.Scope{}) {
		return agentScope
	}
	return s
}

// withPersona prefixes an optional identity block onto the base system prompt,
// so a caller can give the agent a stable character (Config.Persona) that leads
// the tool-using instructions. Empty persona returns base unchanged.
func withPersona(persona, base string) string {
	if persona == "" {
		return base
	}
	return persona + "\n\n" + base
}

// envOr returns the value of an environment variable, or a fallback when unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
