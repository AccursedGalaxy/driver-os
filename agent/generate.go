package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// ---- small helpers ----

// noteUsage reports the run's CUMULATIVE token spend after each turn through the
// Observer (never stdout — the data channel). This is the measurement that makes
// MaxTotalTokens tunable: watch a few runs, then pick a cap. Skipped when the
// provider reports no usage (nothing to measure) — shared by both loops.
func noteUsage(obs Observer, iter int, u llm.Usage, budget int) {
	if u.TotalTokens == 0 {
		return
	}
	msg := fmt.Sprintf("tokens: %d cumulative after turn %d (prompt %d, completion %d)", u.TotalTokens, iter, u.PromptTokens, u.CompletionTokens)
	if budget > 0 {
		msg += fmt.Sprintf(" — budget %d", budget)
	}
	obs.Note(msg)
}

func usageReported(u llm.Usage) bool {
	return u.PromptTokens != 0 || u.CompletionTokens != 0 || u.TotalTokens != 0 || u.CachedTokens != 0 || u.ReasoningTokens != 0
}

// dollarBudgetStop checks Config.MaxTotalCostUSD at the same turn boundary as
// MaxTotalTokens. It prices cumulative usage, so a crossing turn is already in
// res.Usage and is not discarded; callers pass turns=i-1 from the loop header.
func dollarBudgetStop(cfg Config, u llm.Usage, turns int, missingNoted *bool) (bool, string) {
	if cfg.MaxTotalCostUSD <= 0 {
		return false, ""
	}

	var (
		cost float64
		ok   bool
	)
	if cfg.Spend != nil {
		cost, ok = cfg.Spend.USD()
	} else {
		if !usageReported(u) {
			return false, ""
		}
		if cfg.CostFn == nil {
			if !*missingNoted {
				cfg.Obs.Note(fmt.Sprintf("dollar budget %.6f configured but no CostFn is available; continuing without dollar enforcement", cfg.MaxTotalCostUSD))
				*missingNoted = true
			}
			return false, ""
		}
		cost, ok = cfg.CostFn(u)
	}
	if !ok {
		if !*missingNoted {
			cfg.Obs.Note(fmt.Sprintf("dollar budget %.6f configured but cost could not be priced; continuing without dollar enforcement", cfg.MaxTotalCostUSD))
			*missingNoted = true
		}
		return false, ""
	}
	if cost >= cfg.MaxTotalCostUSD {
		return true, fmt.Sprintf("hit dollar budget ($%.6f spent >= cap $%.6f) after %d turn(s)", cost, cfg.MaxTotalCostUSD, turns)
	}
	return false, ""
}

// addUsage sums two Usage values field-by-field so RunResult.Usage accumulates
// the whole run's token cost.
func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		CachedTokens:     a.CachedTokens + b.CachedTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		Cost:             a.Cost + b.Cost,
	}
}

// truncate is the dispatch-level BACKSTOP (P1): per-tool shaping should keep
// output well under observationCap, so this rarely fires — it just guarantees no
// single observation can blow the window.
func truncate(s string) string { return clip(s, observationCap) }

// generateWithEviction sends the FULL context (P1) and applies HP-1's REACTIVE
// fallback on a context-window overflow: rather than crash the run, it evicts the
// oldest tool turn (pairing-safe — see evictOldestTurn) and retries. It returns
// the possibly-shrunk message slice so the caller carries the compaction forward
// into later turns (not just this one). This is deliberately NOT a context POLICY
// (the policy question is HP-1, parked: we're nowhere near the window on real
// tasks — measured ≤10k tokens of a 200k+ window). It is the cheap safety net
// that turns a latent hard crash into graceful degradation IF a transcript ever
// does overflow. A non-overflow error is returned untouched; an overflow that
// can't be compacted any further — or is still overflowing after
// evictionMaxRetries paid attempts — is returned AS llm.ErrContextLength so the
// caller degrades to HitContextLimit instead of mislabelling it a transport fault.
func generateWithEviction(ctx context.Context, cfg Config, req llm.Request) (*llm.Response, []llm.Message, error) {
	for attempt := 0; ; attempt++ {
		resp, err := generateWithRetry(ctx, cfg, req)
		if err == nil || !errors.Is(err, llm.ErrContextLength) {
			return resp, req.Messages, err
		}
		if attempt >= evictionMaxRetries {
			return nil, req.Messages, err // still overflowing after the paid retries — degrade.
		}
		shrunk, evicted := evictOldestTurns(req.Messages)
		if evicted == 0 {
			return nil, req.Messages, err // can't compact further — let the caller degrade.
		}
		cfg.Obs.Note(fmt.Sprintf("context overflow — evicted %d oldest turn(s) (%d→%d msgs), retry %d/%d (HP-1 reactive fallback)",
			evicted, len(req.Messages), len(shrunk), attempt+1, evictionMaxRetries))
		req.Messages = shrunk
	}
}

// evictionMaxRetries caps how many compaction+retry cycles ONE overflow can
// trigger (review #10). Every retry is a fully BILLED model call on a still-
// large transcript, and the old one-turn-per-retry loop had no cap — an N-turn
// overflow could bill up to N sequential calls. Halving the removable turns per
// retry converges in O(log N); three paid attempts is enough for any transcript
// that can converge at all, after which the run degrades to HitContextLimit.
const evictionMaxRetries = 3

// evictOldestTurns drops the oldest HALF of the removable turns (always at
// least one), preserving the same invariants as evictOldestTurn: the TASK
// preamble, the MOST RECENT turn, and tool-call/result pairing. Returns the
// shrunk slice and how many turns were evicted (0 = nothing removable).
func evictOldestTurns(msgs []llm.Message) ([]llm.Message, int) {
	turns := 0
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == llm.RoleAssistant {
			turns++
		}
	}
	removable := turns - 1 // the most recent turn is never evicted (live grounding, P4).
	if removable <= 0 {
		return msgs, 0
	}
	target := (removable + 1) / 2
	evicted := 0
	for evicted < target {
		shrunk, ok := evictOldestTurn(msgs)
		if !ok {
			break
		}
		msgs = shrunk
		evicted++
	}
	return msgs, evicted
}

// transientMaxRetries caps how many RE-SENDS one model call gets after a
// retryable provider fault (each is a fully billed attempt); retryBackoffBase
// spaces them (2s, then 4s — a var so tests can shrink it). Sized for the
// measured failure mode: OpenRouter streams die mid-flight on transient
// upstream faults ({"code":504,"message":"Upstream idle timeout exceeded"},
// run 20260702-204848) and one unretried fault used to kill the WHOLE run,
// discarding every prior iteration's progress.
const transientMaxRetries = 2

var retryBackoffBase = 2 * time.Second

// generateWithRetry re-sends a model call after a RETRYABLE provider error
// (transient 5xx / mid-stream fault / rate limit — the adapter's
// classification). A failed attempt never commits a turn, so the verbatim
// re-send is safe; the partial response of the LAST attempt still rides back
// with the error so the loops' answer salvage keeps what the model already
// said. Non-retryable errors (canceled, auth, context length — the eviction
// wrapper's case) pass through untouched on the first attempt.
func generateWithRetry(ctx context.Context, cfg Config, req llm.Request) (*llm.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := generateOnce(ctx, cfg, req)
		var perr *llm.ProviderError
		if err == nil || !errors.As(err, &perr) || !perr.Retryable || attempt >= transientMaxRetries {
			return resp, err
		}
		delay := retryBackoffBase << attempt
		cfg.Obs.Note(fmt.Sprintf("transient provider error — retry %d/%d in %s: %v",
			attempt+1, transientMaxRetries, delay, err))
		select {
		case <-ctx.Done(): // interrupted mid-backoff: surface the provider error, not a sleep artifact.
			return resp, err
		case <-time.After(delay):
		}
	}
}

// generateOnce performs a single model call — the seam where streaming plugs in.
// When Config.Stream is set AND the provider supports streaming, it consumes the
// stream and pushes incremental text deltas to the observer (DeltaObserver),
// collapsing the chunks into the same *llm.Response shape Generate returns so the
// loop body is identical either way. Otherwise it is a plain Generate call. The
// eviction retry in generateWithEviction wraps this, so a streamed context
// overflow still triggers compaction and a re-stream.
//
// Reasoning traces: adapters yield the turn's reasoning once fully assembled
// (a Reasoning chunk) and collectStream keeps it, so streaming replays with
// full fidelity — anthropic's API REJECTS a replayed tool-using turn missing
// its signed thinking blocks, and openaicompat reassembles the streamed
// `reasoning_details` fragments (Gemini's encrypted signature) the same way.
func generateOnce(ctx context.Context, cfg Config, req llm.Request) (*llm.Response, error) {
	if cfg.Stream && cfg.Model.Capabilities().Streaming {
		return collectStream(cfg.Model.Stream(ctx, req), deltaSink(cfg.Obs))
	}
	return cfg.Model.Generate(ctx, req)
}

// collectStream drains a chunk stream into a Response, invoking onDelta (if
// non-nil) for each text fragment as it arrives — the live-typing tap. It mirrors
// Generate's output: accumulated text becomes a single leading Text part, then the
// fully-assembled tool calls in arrival order, with FinishReason and Usage taken
// from the terminal Done chunk. A streamed error aborts and propagates (the
// eviction wrapper classifies ErrContextLength) — but the PARTIAL response
// collected so far rides back WITH the error: the turn is not committed, yet the
// prose that already streamed often carries the model's diagnosis, and the loops'
// answer salvage must not lose it. Reasoning chunks (assembled traces an adapter
// yields for replay — see llm.Chunk) are kept, leading the Content the way a
// thinking provider orders them; a stream that dies BEFORE its end-of-stream
// Reasoning chunk salvages without a trace — correct, since a partial trace
// (a truncated signature) must never be replayed.
func collectStream(seq iter.Seq2[llm.Chunk, error], onDelta func(string)) (*llm.Response, error) {
	var text strings.Builder
	var calls, reasoning []llm.ContentPart
	resp := &llm.Response{}
	assemble := func() *llm.Response {
		resp.Content = append(resp.Content, reasoning...)
		if text.Len() > 0 {
			resp.Content = append(resp.Content, llm.Text(text.String()))
		}
		resp.Content = append(resp.Content, calls...)
		return resp
	}
	for chunk, err := range seq {
		if err != nil {
			return assemble(), err
		}
		switch {
		case chunk.Done:
			resp.FinishReason = chunk.FinishReason
			resp.Usage = chunk.Usage
		case chunk.Reasoning != nil:
			reasoning = append(reasoning, *chunk.Reasoning)
		case chunk.ToolCall != nil:
			calls = append(calls, *chunk.ToolCall)
		case chunk.Text != "":
			text.WriteString(chunk.Text)
			if onDelta != nil {
				onDelta(chunk.Text)
			}
		}
	}
	return assemble(), nil
}

// evictOldestTurn is HP-1's reactive compaction unit: it drops the OLDEST tool
// turn from the transcript while preserving (a) the opening TASK message and any
// preamble before the first assistant turn, (b) the MOST RECENT turn (the live
// grounding — P4), and (c) tool-call/tool-result pairing. The pairing constraint
// is why the unit is a whole turn, never a lone observation: in the native loop a
// RoleTool result is only well-formed immediately after the assistant message
// that requested it, so [first assistant message, next assistant message) is the
// smallest safely-removable span. The same span rule works for the text loop,
// where a turn is Assistant(reply) + User(OBSERVATION). Returns the shrunk slice
// and true, or the input unchanged and false when only TASK + one turn remain
// (nothing left to drop without losing the most recent grounding).
func evictOldestTurn(msgs []llm.Message) ([]llm.Message, bool) {
	a := -1 // start of the oldest turn: the first assistant message after the preamble.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == llm.RoleAssistant {
			a = i
			break
		}
	}
	if a < 0 {
		return msgs, false // no assistant turn yet — nothing to evict.
	}
	b := len(msgs) // end of the oldest turn: the NEXT assistant message (turn boundary).
	for i := a + 1; i < len(msgs); i++ {
		if msgs[i].Role == llm.RoleAssistant {
			b = i
			break
		}
	}
	if b >= len(msgs) {
		return msgs, false // `a` is the only/most-recent turn — keep it for grounding.
	}
	out := make([]llm.Message, 0, len(msgs)-(b-a))
	out = append(out, msgs[:a]...) // TASK + preamble.
	out = append(out, msgs[b:]...) // every turn from the second-oldest onward.
	return out, true
}

// clip bounds a string to capRunes, keeping the HEAD and the TAIL and eliding the
// middle. Head+tail beats head-only for tool output: a build log's failure is at
// the end, and a tool's own "next range" footer lives at the end too — a blind
// head-cut would drop exactly the recovery signal (P1, P3). It is rune-based, not
// byte-based: a Go string is BYTES, so slicing s[:n] can split a multi-byte rune
// and emit invalid UTF-8; converting to runes first makes the cut clean.
func clip(s string, capRunes int) string {
	r := []rune(s)
	if len(r) <= capRunes {
		return s
	}
	head := capRunes * 2 / 3
	tail := capRunes - head
	return string(r[:head]) + fmt.Sprintf("\n...[%d runes elided]...\n", len(r)-head-tail) + string(r[len(r)-tail:])
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	r := []rune(s) // rune-safe slice; same reason as clip.
	if len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}

// toolNames returns tool names in a stable, sorted order so callers never
// depend on Go's randomized map iteration.
func toolNames(tools map[string]Tool) []string {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
