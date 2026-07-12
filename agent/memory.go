// Long-term memory for the agent — the natural extension of Principle 1 (state
// lives in YOUR code) and Principle 3 (context is the only state, so managing it
// IS the engineering). The in-loop `messages` slice is state for ONE run; long-term memory
// is state that survives ACROSS runs. We recall relevant facts before thinking
// and store what we concluded after answering. The model never drives this — the
// harness decides what to remember and what to surface (Principle 7).
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/memory"
)

// memoryTimeout bounds each memory call (a recall search, or a store extraction).
const memoryTimeout = 30 * time.Second

// agentScope namespaces this agent's memories.
var agentScope = memory.Scope{AgentID: "driver-os-agent"}

// recall fetches facts relevant to the task from past runs and renders them as a
// system-prompt block. Returns "" when there is nothing to add (first run, no
// memory, or a recall error — all non-fatal). The block is explicitly labelled
// as possibly-stale so the model still verifies with tools (Principle 4: observe
// REAL state, don't trust prior text). Consolidate-on-write (see the configured memory adapter)
// corrects facts that a LATER grounded run re-observes, but a fact no run has
// revisited can still be stale — so the verify-with-tools framing stays.
func recall(ctx context.Context, obs Observer, mem memory.Store, scope memory.Scope, task string) string {
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
func remember(ctx context.Context, obs Observer, mem memory.Store, scope memory.Scope, task, answer string) {
	if mem == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), memoryTimeout)
	defer cancel()
	written, err := mem.Add(ctx, []memory.Message{
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
//
// CONCURRENT SAFETY (backlog A4): The goroutine this fires calls mem.Add while
// the next turn's recall calls mem.Search on the same store concurrently.
// This is safe: store/sqlite.Open documents the returned Store as "safe for
// concurrent use by multiple goroutines" (WAL mode + busy_timeout(5000)), and
// the mneme Memory interface states it is "safe for concurrent use to the
// extent its underlying store, LLM and embedder are" — the store guarantees
// it, and the LLM/embedder providers are stateless HTTP clients.
// TestConcurrentAddSearchWithRealStore exercises this under -race.
func rememberAsync(ctx context.Context, obs Observer, mem memory.Store, scope memory.Scope, task, answer string) <-chan struct{} {
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
func StoreMemoryAsync(ctx context.Context, obs Observer, mem memory.Store, scope memory.Scope, task, answer string) <-chan struct{} {
	return rememberAsync(ctx, obs, mem, scopeOrDefault(scope), task, answer)
}

// scopeOrDefault resolves the memory namespace for a run: an explicit
// Config.MemoryScope when set, else the package default. This is what lets two
// agents share one store without their facts bleeding together (each passes its
// own scope, e.g. {AgentID: "adam"} vs {AgentID: "alex"}); a lone CLI run that
// sets nothing keeps the historical single-scope behavior.
func scopeOrDefault(s memory.Scope) memory.Scope {
	if s == (memory.Scope{}) {
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
