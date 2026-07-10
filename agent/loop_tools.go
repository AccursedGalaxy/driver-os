package agent

// RunNative is the think -> act -> observe loop driven by the provider's NATIVE
// tool-calling, the structured counterpart to Run's one-line text protocol. It
// exists because the text protocol became the binding constraint on coding tasks
// (DOGFOOD round 5): a model jamming several actions onto one line, and multi-line
// file content escaped through "\n", are both artifacts of squeezing structure
// through plain text. Native tool-calling removes all of it — the model emits
// typed tool calls with JSON args (so content is a real multi-line string, no
// escapes), the harness runs them and replies with typed results, and termination
// is "the model stopped calling tools and answered in plain text".
//
// Everything that IS the agent is shared with Run: the same Config, the same
// Tool map, Observer, memory, termination knobs, and the seven principles. ONLY
// the wire format differs. Run stays as the deliberately-transparent text version
// (and the fallback for tool-less providers); this is the production path for a
// frontier model.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// parallelSafe reports whether a tool has no side effects and no shared state,
// so it can be dispatched concurrently with other such calls in the same turn.
// run/write_file/edit_file are excluded. go_doc reads host Go docs/source only.
func parallelSafe(name string) bool {
	switch name {
	case "read_file", "search", "list_dir", "go_doc":
		return true
	}
	return false
}

type prefetchResult struct {
	obs   string
	isErr bool
	dur   time.Duration // real dispatch wall-time of THIS call (see ToolMs requirement)
}

// cacheStats accumulates per-turn prompt-cache efficiency metrics over a run.
// It matches the formula in eval/scripts/read_dup_pass.py Measurement 3 exactly:
// for turn i with prompt P_i and actual cached C_i:
//
//	expected_i = (i == 1) ? 0 : min(P_{i-1}, P_i)
//	miss_i     = max(0, expected_i − C_i)
//
// The struct tracks the previous turn's P for the next expected calculation.
type cacheStats struct {
	prevPromptTokens  int
	SumExpectedCached int
	SumActualCached   int
	CacheMiss         int
}

// observe records one turn and returns the per-turn (expected, actual, miss).
func (cs *cacheStats) observe(promptTokens, cachedTokens int) (expected, actual, miss int) {
	expected = 0
	if cs.prevPromptTokens > 0 {
		expected = min(cs.prevPromptTokens, promptTokens)
	}
	actual = cachedTokens
	miss = max(0, expected-actual)
	cs.SumExpectedCached += expected
	cs.SumActualCached += actual
	cs.CacheMiss += miss
	cs.prevPromptTokens = promptTokens
	return
}

// hitPct returns the run-level cache-hit percentage, or 0 when no expected
// cached tokens have been accumulated.
func (cs *cacheStats) hitPct() float64 {
	if cs.SumExpectedCached == 0 {
		return 0
	}
	return 100.0 * float64(cs.SumActualCached) / float64(cs.SumExpectedCached)
}

// prefetchLeadingReadOnly dispatches the maximal LEADING prefix of parallel-safe
// calls concurrently and returns their results indexed by position plus the
// prefix length k. It prefetches only when k >= 2 AND the leading call would not
// be elided by the identical-read skip (skipLeading == false); otherwise it
// returns (nil, 0) and the caller dispatches every call inline as today.
// results[i] is valid only for i in [0,k); ordering matches `calls`; physical
// execution order does not. Each goroutine records its own call's dispatch
// duration in results[i].dur.
func prefetchLeadingReadOnly(ctx context.Context, tools map[string]Tool, calls []llm.ToolCallPart, skipLeading bool) (results []prefetchResult, k int) {
	for _, c := range calls {
		if !parallelSafe(c.Name) {
			break
		}
		k++
	}
	if k < 2 || skipLeading {
		return nil, 0
	}
	results = make([]prefetchResult, k)
	var wg sync.WaitGroup
	wg.Add(k)
	for i := 0; i < k; i++ {
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			obs, isErr := dispatchNative(ctx, tools, calls[i])
			results[i] = prefetchResult{obs, isErr, time.Since(start)}
		}(i)
	}
	wg.Wait()
	return results, k
}

// RunNative executes the agent against a tool-capable provider. Its signature and
// RunResult match Run exactly, so a caller swaps loops without other changes.
func RunNative(ctx context.Context, cfg Config) (out *RunResult, err error) {

	ev := &evidenceLog{root: cfg.Root, verifyConfigured: strings.TrimSpace(cfg.VerifyCmd) != "", reviewConfigured: cfg.Reviewer != nil, costConfigured: cfg.MaxTotalCostUSD > 0}
	cfg.evidence = ev
	defer func() {
		if out != nil {
			out.Guarantees = finalizeGuarantees(ev, out.Outcome, out.Review)
		}
	}()
	// (P1 spine) Same run-identity stamping as Run, registered BEFORE the isolation
	// refusal so a refused run is addressable too (its transcript write won't fail
	// on an empty ID).
	runID, startedAt := newRunID(), time.Now()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// (N1) Last prose the model produced this run, captured even on iterations that
	// ALSO called tools (native termination only records prose on a no-tool turn).
	// The defer salvages it as the answer when the run ends WITHOUT a clean answer —
	// a hit_cap/killed turn often still narrated something useful, and a caller that
	// relays the answer as a message (duet) should get that text, not silence
	// (DUET-DOGFOOD N1). Outcome is untouched: this only fills an empty Answer.
	var lastAssistantText string
	defer func() {
		if out != nil && out.Answer == "" && out.Outcome != Answered && lastAssistantText != "" {
			out.Answer = lastAssistantText
		}
		stampRun(out, runID, startedAt)
	}()
	if refusal := checkIsolation(cfg); refusal != nil {
		ev.isolation = EvidenceFailed
		// Prompt resolution and native schemas have not run on this safety refusal;
		// the record deliberately hashes empty protocol representations.
		refusal.ConfigRecord = newConfigRecord(cfg, "", nil, "tools")
		return refusal, nil // (P2/§5) too-weak sandbox — refuse before the first model call.
	}
	ev.isolation = EvidencePassed
	// (PROMPT-SKILLS slices 2+3) Resolve the base prompt BEFORE any paid call
	// (the plan stage bills first) — an unknown profile must abort, not run
	// mislabeled. Once Obs exists, its routing note is emitted for every run that
	// reaches prompt setup; pre-prompt refusals instead carry the empty-representation
	// ConfigRecord documented in BINARY-UNIFICATION.md.
	basePrompt, promptNote, err := resolveSystemPrompt(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}
	if promptNote != "" {
		cfg.Obs.Note(promptNote)
	}
	resolveAutoVerify(ctx, &cfg)
	ev.verifyConfigured = strings.TrimSpace(cfg.VerifyCmd) != ""
	ev.verifyCommand = cfg.VerifyCmd
	knobs := resolveKnobs(cfg)
	maxIter, maxTok, runTimeout, spiralWindow := knobs.maxIter, knobs.maxTok, knobs.runTimeout, knobs.spiralWindow
	cfg.Tools = wrapTools(cfg, runTimeout)
	gs, err := newGates(ctx, cfg, runTimeout)
	if err != nil {
		return nil, err
	}
	ev.baseTree = gs.runBaseTree
	ev.closingReady = true
	cfg.Tools = gs.addReproTools(cfg.Tools)

	// The answer-forcer (AnswerNudgeWindow) is only SAFE for an OBSERVE-ONLY agent: one
	// that can't have left work half-done, so a forced "stop and answer" can't mask an
	// unverified broken state. We do NOT extend it to verified coding runs — VerifyLastRun
	// misses a passed-then-edited-broken run, and VerifyCmd-without-VerifyContinue would
	// burn the repair turns (dogfood slice 4, round 3, O1/O2). isObserveOnly is an
	// ALLOWLIST (every tool is a known read-only built-in) so it FAILS CLOSED: a custom
	// effectful tool the harness doesn't recognize makes the run not-observe-only and the
	// nudge stays out (O3). Coding runs use FinishNudgeWindow instead.
	answerNudgeOK := isObserveOnly(cfg.Tools)

	res := &RunResult{Task: cfg.Task, Root: cfg.Root}
	recordAutoVerifyResolution(res, cfg)
	gs.applyBaseline(res)
	if refusal := redBaselineRefusal(cfg, gs); refusal != nil {
		res.Outcome, res.Reason, res.Iterations = refusal.Outcome, refusal.Reason, refusal.Iterations
		return res, nil
	}
	// The review report travels on EVERY exit path (findings + fates are the
	// calibration telemetry) — nil when the gate is off.
	defer func() {
		if out != nil {
			gs.applyVerifyInfra(out)
			out.Review = gs.reviewReport()
			out.Repro = gs.reproReport()
		}
	}()

	// (TRIAD) The opening PLAN stage — see Run's twin for the ordering notes
	// (after newGates so the reviewer judges the original task; fails open).
	planTask, planRep := runPlanStage(ctx, cfg)
	res.Plan = planRep
	seedCfg := cfg
	seedCfg.Task = planTask

	// (P1) State lives HERE; we re-send the whole conversation each turn. A
	// continuing chat seeds it with the prior turns (Config.History); see Session.
	messages := seedMessages(seedCfg, observeEnvironment(ctx, cfg.Sandbox, cfg.BootContext)+verifyGatePreamble(cfg)+gs.baselinePreamble())
	// Expose the final conversation on every loop exit (the continuation seam, see
	// RunResult.Messages). Separate from the top-of-func salvage defer; this one is
	// registered after `messages` exists so the closure reads its final value.
	defer func() {
		if out != nil {
			out.Messages = messages
		}
	}()
	// (P3) Recalled long-term memory rides in the system prompt, labelled stale.
	scope := scopeOrDefault(cfg.MemoryScope)
	system := withPersona(cfg.Persona, basePrompt) + recall(ctx, cfg.Obs, cfg.Memory, scope, cfg.Task)
	schemas := nativeSchemas(cfg.Tools) // typed per-tool schemas, with a single-`arg` bridge fallback.
	res.ConfigRecord = newConfigRecord(cfg, withPersona(cfg.Persona, basePrompt), schemas, "tools")

	temp := 0.0

	// (P5) No-progress state. Unlike the text loop these are evaluated PER TURN, not
	// per call: a native turn can carry several tool calls at once (parallel
	// fan-out), so a call-by-call detector would mistake one 4-way list_dir turn for
	// a 4-turn spiral. lastTurnSig dedupes whole turns; navRun counts consecutive
	// list_dir-only turns.
	var lastTurnSig string
	var lastReasoningSig string // (P5) the prior turn's reasoning fingerprint — see the repeat detector.
	var lastReadSig string      // (Change A) signature of the last executed read-only call.
	var lastReadObs string      // (Change A) observation from the last executed read-only call.
	var lastExecutedSig string  // (Change A) signature of the immediately-preceding EXECUTED call.
	dd := newObsDedup()
	repeats := 0
	// (P5) Frontier/state-aware explore-spiral detector (deliverable 1): replaces
	// the old fixed consecutive-discovery-turn count. spiralWindow is the cycle
	// window (Config.NavSpiralWindow, default noProgressWindow); see spiralState.
	spiral := newSpiralState(spiralWindow)
	grounded := false // (P4) gates memory writes — only a verified answer is stored.

	// (review #9) The stagnant/churn/diagnostics/finisher state shared with Run
	// lives in ONE tracker; the vars above stay loop-local because they are
	// wire-format-specific (per-turn here, per-call in the text loop). The
	// stagnant detector is still evaluated on each `run` result regardless of
	// which turn it lands in.
	tr := newTurnTracker(cfg, maxIter)
	var standing *standingState
	if cfg.StandingContext {
		standing = newStandingState()
		defer standing.cleanup()
	}
	answerNudged := false
	sayNudged := false

	start := time.Now() // (P5) wall-clock budget anchor; see MaxWallClock.
	// A deadline-bound context so a slow IN-FLIGHT call (a reasoning-heavy Generate,
	// a hung provider) is cancelled AT the budget — the between-turn check alone
	// can't interrupt a call already in progress (the gpt-oss exit-124 case).
	loopCtx := ctx
	if cfg.MaxWallClock > 0 {
		var cancel context.CancelFunc
		loopCtx, cancel = context.WithTimeout(ctx, cfg.MaxWallClock)
		defer cancel()
	}

	var costBudgetMissingNoted bool
	var contextEstimateNoted bool
	cs := &cacheStats{} // prompt-cache efficiency instrumentation per Measurement 3

	for i := 1; i <= maxIter; i++ {
		if errors.Is(ctx.Err(), context.Canceled) {
			res.Outcome = Canceled
			res.Reason = "run canceled by the caller (interrupt)"
			res.Iterations = i - 1
			return res, nil
		}
		if cfg.MaxWallClock > 0 && time.Since(start) > cfg.MaxWallClock {
			res.Outcome = HitDeadline
			res.Reason = fmt.Sprintf("hit wall-clock budget (%s) after %d turn(s)", cfg.MaxWallClock, i-1)
			return gs.upgradeIfVerified(ctx, res), nil
		}
		// (P5/HP-8) Token budget, checked at the turn boundary like the wall-clock:
		// the turn that crossed the cap was still processed (it may have answered);
		// the NEXT one is not paid for. See Config.MaxTotalTokens.
		if cfg.MaxTotalTokens > 0 && res.Usage.TotalTokens >= cfg.MaxTotalTokens {
			res.Outcome = HitBudget
			res.Reason = fmt.Sprintf("hit token budget (%d total tokens >= cap %d) after %d turn(s)", res.Usage.TotalTokens, cfg.MaxTotalTokens, i-1)
			return gs.upgradeIfVerified(ctx, res), nil
		}
		if stop, reason := dollarBudgetStop(cfg, res.Usage, i-1, &costBudgetMissingNoted); stop {
			res.Outcome = HitBudget
			res.Reason = reason
			return gs.upgradeIfVerified(ctx, res), nil
		}
		cfg.Obs.Iteration(i, maxIter)

		standingBlock := ""
		if cfg.StandingContext && standing != nil {
			standingBlock = standing.block(loopCtx, cfg, gs, tr, i)
		}

		// generateWithEviction adds HP-1's reactive fallback: on a window overflow it
		// compacts the OLDEST turn (pairing-safe) and retries instead of crashing,
		// returning the possibly-shrunk transcript we carry forward.
		var resp *llm.Response
		var err error
		modelStart := time.Now()
		req := llm.Request{
			System:          system,
			Messages:        messages,
			StandingContext: standingBlock,
			Tools:           schemas,
			Temperature:     &temp,
			MaxTokens:       maxTok,
			ReasoningEffort: cfg.ReasoningEffort,
		}
		noteContextEstimate(cfg, req, &contextEstimateNoted)
		resp, messages, err = generateWithEviction(loopCtx, cfg, req)
		modelMs := time.Since(modelStart).Milliseconds()
		if err != nil {
			// A streamed turn that died mid-flight still returns the prose collected
			// before the fault (collectStream). Feed it to the N1 salvage so every
			// typed stop below carries what the model already said instead of silence
			// — the turn itself is NOT committed to messages.
			if resp != nil {
				if txt := strings.TrimSpace(resp.Text()); txt != "" {
					lastAssistantText = txt
				}
			}
			if outcome, reason, returnErr, ok := classifyGenerateErr(ctx, loopCtx, cfg, err); ok {
				res.Outcome, res.Reason, res.Err = outcome, reason, returnErr
				if returnErr != nil {
					return res, returnErr
				}
				if outcome == Canceled {
					return res, nil
				}
				return gs.upgradeIfVerified(ctx, res), nil
			}
		}
		res.Iterations = i
		res.Usage = addUsage(res.Usage, resp.Usage)
		cfg.Spend.Add(cfg.SolverModel, resp.Usage) // nil-safe; per-turn solver dollars
		noteUsage(cfg.Obs, i, res.Usage, cfg.MaxTotalTokens)
		ctxTok := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		notifyUsage(cfg.Obs, res.Usage, ctxTok)

		// Prompt-cache efficiency tracking (Measurement 3).
		cs.observe(resp.Usage.PromptTokens, resp.Usage.CachedTokens)
		res.CacheSumExpectedCached = cs.SumExpectedCached
		res.CacheSumCached = cs.SumActualCached
		res.CacheMiss = cs.CacheMiss
		res.CacheHitPct = cs.hitPct()

		// (P5) Hidden-reasoning progress for THIS turn, computed once: it selects the
		// tight-loop threshold below AND is recorded on every Step of the turn so a
		// transcript can show when the lenient ceiling applied. Empty trace (a
		// non-thinking model) => not advanced, so the detector stays strict.
		//
		// Deliberately NOT gated on ReasoningTokens > 0 (DUET-DOGFOOD N3, tried and
		// REVERTED 2026-06-12): gemini via OpenRouter moves its encrypted
		// thought-signature every call while reporting reasoning_tokens=0, so the
		// gate read it as a nonce and dropped it to the strict ceiling — which
		// false-killed its digest-re-read pattern (read_file item.go ×3 while
		// walking a call chain in its head) and regressed the trace eval 5/5 → 0/5
		// (eval/runs/n3gate-trace-gemini, killed_repeat=4). The zero-token signature
		// movement IS thought for that provider. N3's measured harm (a byte-identical
		// no-op idle loop riding the lenient ceiling to the cap) was downstream of
		// N2 — no first-class way to end the turn — and is fixed at the root by the
		// finish/`say` tool, not here.
		reasoningSig := reasoningSignature(resp.Content)
		reasoningAdvanced := reasoningSig != "" && reasoningSig != lastReasoningSig

		calls := toolCalls(resp.Content)
		// The model's prose narration for THIS turn. Recorded on the first step of
		// every turn (review #6) — a tool turn often narrates intent/conclusions,
		// and a transcript that only shows what the model DID loses what it SAID.
		turnReply := strings.TrimSpace(resp.Text())

		// (N1) Remember the latest non-empty prose, even when this turn also calls
		// tools — the salvage source if the run never reaches a clean answer.
		if turnReply != "" {
			lastAssistantText = turnReply
		}

		// (P5) Termination: the model stopped calling tools and answered in prose.
		if len(calls) == 0 {
			answer := strings.TrimSpace(resp.Text())
			// (FinishTool) An EMPTY no-tool-call turn is the model going silent. For a
			// caller whose finish IS a message (duet's say), accepting it as a clean
			// "answer" reintroduces the silent-turn failure through the prose path. While
			// budget remains, reject it and nudge the model to finish via the finish tool
			// instead. No FinishTool configured => the historical behavior (an empty
			// answer is accepted), so no other caller is affected.
			if answer == "" && cfg.FinishTool != "" && i < maxIter {
				messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})
				// A reasoning model (deepseek-v4-flash via openaicompat) routinely emits a
				// THINK-ONLY turn — reasoning advanced, but no text and no tool call yet —
				// then acts on the next turn. That is mid-thought, NOT a finish attempt, so
				// the say-nudge below mis-instructs a model that isn't done (and burns a
				// round-trip telling it to wrap up). Carry the reasoning forward (it's
				// already appended above, preserving the thought trace) and continue
				// silently. The iteration cap still bounds it; advancing lastReasoningSig
				// here means a turn whose trace then FROZEN reads as not-advanced next time
				// and falls through to the genuine-silence nudge.
				if reasoningAdvanced {
					lastReasoningSig = reasoningSig
					cfg.Obs.Note("empty turn — reasoning advanced, continuing")
					continue
				}
				messages = append(messages, llm.User(fmt.Sprintf("You ended your turn without saying anything. Use the %q tool to send a short message — that is how you finish your turn.", cfg.FinishTool)))
				cfg.Obs.Note("empty finish — nudging to use the " + cfg.FinishTool + " tool")
				continue
			}
			res.Steps = append(res.Steps, Step{Iter: i, Reply: answer, Verb: "answer", Arg: answer, Grounded: grounded, Usage: resp.Usage, ModelMs: modelMs, ReasoningAdvanced: reasoningAdvanced, FinishReason: resp.FinishReason})
			// (P1/continuation seam) The model's final tool-call-free turn is part of the
			// conversation a Session carries forward — append it before any terminal
			// return so RunResult.Messages ends with the answer the model gave.
			messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})
			dec := gs.finish(ctx, finishInput{
				answer:               answer,
				lastRunFailed:        tr.lastRunFailed,
				canContinue:          i < maxIter,
				verifyContinuePhrase: "you stopped calling tools",
				grounded:             grounded,
				memoryScope:          scope,
				unverifiedNotePrefix: "answer not verified",
				reviewBlockedPrefix:  "answer blocked by review",
			})
			switch dec.kind {
			case finishContinue:
				messages = append(messages, llm.User(dec.feedback))
				continue
			case finishStop:
				res.Outcome, res.Answer, res.Reason = dec.outcome, dec.answer, dec.reason
				return res, nil
			case finishAnswered:
				res.Outcome, res.Answer = Answered, dec.answer
				res.memDone = dec.memDone
				return res, nil
			}
		}

		cfg.Obs.Model(callsSummary(calls, cfg.Tools))

		// (FinishTool) First-class terminal finish: the model called the designated
		// finish tool to deliberately end its turn (see Config.FinishTool). Any other
		// calls in the same turn run first so a last cp/build still lands, then we
		// terminate cleanly as Answered with the finish tool's message. Usage/latency
		// follow the per-turn convention: attributed to the first recorded step only.
		//
		// The assistant turn is appended HERE (not unconditionally before the
		// detectors) so a no-progress kill below returns a well-formed transcript
		// instead of a dangling assistant tool-call — the continuation seam bug
		// (harness review finding #1). The finish path needs it appended before its
		// tool results, and pairs the finish call itself with a synthetic result so no
		// tool call is left unanswered when a Session continues past the finish.
		if cfg.FinishTool != "" {
			if fin := findCall(calls, cfg.FinishTool); fin != nil {
				messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})
				usageForFinish, modelMsForFinish := resp.Usage, modelMs
				replyForFinish := turnReply // the turn's narration rides its first recorded step (review #6).
				for _, c := range calls {
					if c.ID == fin.ID {
						continue
					}
					step := Step{Iter: i, Verb: c.Name, Arg: callArg(c, cfg.Tools), Usage: usageForFinish, ModelMs: modelMsForFinish, Reply: replyForFinish, ReasoningAdvanced: reasoningAdvanced, FinishReason: resp.FinishReason}
					usageForFinish, modelMsForFinish, replyForFinish = llm.Usage{}, 0, ""
					obs, isErr := dispatchNative(loopCtx, cfg.Tools, c)
					if isMutatingTool(c.Name) && !isErr {
						ev.mutation()
					}
					if !isErr {
						grounded = true
					}
					if c.Name == "run" {
						tr.observeRun(obs) // so a VerifyLastRun gate reads this turn's last run (kill ignored — we're finishing).
						recordNativeRun(loopCtx, cfg, standing, tr, c, obs)
					}
					step.Grounded = grounded
					step.Observation = obs
					res.Steps = append(res.Steps, step)
					cfg.Obs.Observation(obs)
					messages = append(messages, llm.ToolResultMsg(c.ID, obs, isErr))
				}
				msg := finishToolMessage(*fin)
				// Pair the finish call with a synthetic result (finding #1): its own
				// dispatch is a no-op terminal signal, but the transcript must not carry
				// an unanswered tool call.
				messages = append(messages, llm.ToolResultMsg(fin.ID, msg, false))
				res.Steps = append(res.Steps, Step{Iter: i, Reply: msg, Verb: fin.Name, Arg: msg, Grounded: grounded, Usage: usageForFinish, ModelMs: modelMsForFinish, ReasoningAdvanced: reasoningAdvanced, FinishReason: resp.FinishReason})
				dec := gs.finish(ctx, finishInput{
					answer:               msg,
					lastRunFailed:        tr.lastRunFailed,
					canContinue:          i < maxIter,
					trusted:              cfg.FinishToolTrustsCaller,
					verifyContinuePhrase: "you called the finish tool",
					grounded:             grounded,
					memoryScope:          scope,
					unverifiedNotePrefix: "finish not verified",
					reviewBlockedPrefix:  "finish blocked by review",
				})
				switch dec.kind {
				case finishContinue:
					messages = append(messages, llm.User(dec.feedback))
					continue
				case finishStop:
					res.Outcome, res.Answer, res.Reason = dec.outcome, dec.answer, dec.reason
					return res, nil
				case finishAnswered:
					res.Outcome, res.Answer = Answered, dec.answer
					res.memDone = dec.memDone
					return res, nil
				}
			}
		}

		// (P5) Two no-progress detectors, the same intent as the text loop but
		// evaluated PER TURN (a native turn may legitimately fan out several calls):
		// (a) tight loop — the model re-issues the IDENTICAL turn over and over;
		// (b) explore-spiral — consecutive turns that do nothing but DISCOVER with
		//     list_dir/search, never escalating to run/read_file/edit or answering. A
		//     turn that mixes discovery with any other tool is progress and resets the
		//     count, and a single turn that fans out N parallel discovery calls is ONE
		//     nav turn, not N — so real parallel exploration is never mistaken for a
		//     spiral. (b) keys on the discovery CLASS so search-churn and mixed
		//     list_dir/search wandering trip the same net the list_dir-only case did.
		sig := turnSignature(calls, cfg.Tools)
		// A repeat counts toward the tight-loop kill ONLY if the model's reasoning
		// also didn't advance. A thinking model (Gemini) re-issues the SAME visible
		// action across turns while its encrypted chain of thought moves forward — the
		// action repeats but real, hidden progress is happening, and it converges given
		// room (the Gemini repofix diagnosis: instakilling at maxRepeats=2 was a false
		// positive). A non-reasoning model returns no reasoning, so reasoningSig is ""
		// every turn and this reduces to the old action-only detector exactly. A
		// reasoning model that has TRULY stalled (its trace froze too) still trips it;
		// the iteration cap + wall-clock + failing-run/list_dir detectors remain the
		// backstops for a thinking model that wanders without ever stalling its trace.
		if sig == lastTurnSig { // (a)
			// Pick the threshold by whether the model is actively reasoning: a turn
			// whose reasoning trace MOVED gets the lenient ceiling (hidden progress —
			// don't false-kill it mid-thought), a frozen or absent trace gets the strict
			// one (a true tight loop). Either way the spiral is bounded — the lenient
			// ceiling still cuts a 20x re-read long before the iteration cap.
			if repeats++; repeats >= repeatThreshold(reasoningAdvanced) {
				steps := turnSteps(i, calls, cfg.Tools, grounded, resp.Usage, modelMs, reasoningAdvanced, turnReply, resp.FinishReason)
				res.Reason = fmt.Sprintf("no progress: repeated %q %d times", sig, repeats)
				// (deliverable 5) explicit killing-turn Observation, uniform with the
				// spiral kill below — an empty turn reads as a broken checkout in traces.
				if len(steps) > 0 {
					steps[0].Observation = "harness: run killed by tight-loop detector: " + res.Reason
				}
				res.Steps = append(res.Steps, steps...)
				res.Outcome = KilledRepeat
				return gs.upgradeIfVerified(ctx, res), nil
			}
		} else {
			repeats = 0
		}
		lastTurnSig = sig
		lastReasoningSig = reasoningSig

		if allCallsDiscovery(calls) { // (b)
			// Frontier/state-aware policy (deliverable 1): a discovery turn that
			// reveals a NEW list_dir path or search query is orientation and never
			// counts toward the kill — this is what lets a textbook top-down
			// orientation of a large repo (list_dir . → pkg → pkg/sub → targeted
			// search, all novel) run to its first useful read instead of dying at a
			// fixed window (the opa-8781 P1 false-kill). A turn revisiting only
			// already-seen targets is a cycle and counts toward the window; endless
			// novel wandering still dies at the hard bound. Bounds are deterministic
			// — no model-family or reasoning variance (spiralState).
			if kill, reason := spiral.observeDiscoveryTurn(discoveryTargets(calls, cfg.Tools)); kill {
				steps := turnSteps(i, calls, cfg.Tools, grounded, resp.Usage, modelMs, reasoningAdvanced, turnReply, resp.FinishReason)
				// (deliverable 5) Record an explicit Observation on the killing turn so
				// the trace doesn't read as a broken checkout / empty turn.
				if len(steps) > 0 {
					steps[0].Observation = "harness: run killed by explore-spiral detector: " + reason
				}
				res.Steps = append(res.Steps, steps...)
				res.Outcome = KilledSpiral
				res.Reason = reason
				return gs.upgradeIfVerified(ctx, res), nil
			}
		} else {
			// Any non-discovery turn (the first read_file, a run/diagnostics call, or
			// a workspace mutation) is a phase transition: clear the frontier and both
			// counters so the model earns a fresh orientation budget (deliverable 1a/1e).
			spiral.reset()
		}

		// (P1) Committed to dispatch — NOW the assistant turn (carrying its tool-call
		// parts) becomes state we re-send; the API requires the tool_calls to precede
		// their results. Deferred to here (past the no-progress detectors above) so a
		// detector kill returns a well-formed transcript with no message-order error.
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})

		// The identical-read skip (below) elides a leading read that repeats the
		// previous turn's read. If calls[0] would be skipped, disable prefetch for
		// this turn: that is the "model is repeating" case, never the parallel-
		// orientation case we optimize, and prefetching it would defeat the skip.
		skipLeading := false
		if len(calls) > 0 {
			c0 := calls[0]
			readOnly0 := c0.Name == "read_file" || c0.Name == "search" || c0.Name == "list_dir"
			if readOnly0 && !reasoningAdvanced {
				sig0 := c0.Name + " " + signatureArg(c0, cfg.Tools)
				skipLeading = sig0 == lastReadSig && sig0 == lastExecutedSig
			}
		}
		prefetched, prefetchK := prefetchLeadingReadOnly(loopCtx, cfg.Tools, calls, skipLeading)

		// ACT: run every requested call in order, appending one result per id (a
		// missing result for any call would make the next request malformed).
		usageForTurn := resp.Usage // attribute the turn's usage to its first step only.
		modelMsForTurn := modelMs  // likewise the model-call latency: a per-turn cost.
		editedThisTurn := false    // (code-intel slice 1) did any call this turn mutate a file?
		turnObservationFPs := make([]string, 0, len(calls))
		for idx, c := range calls {
			step := Step{Iter: i, Verb: c.Name, Arg: callArg(c, cfg.Tools), Usage: usageForTurn, ModelMs: modelMsForTurn, ReasoningAdvanced: reasoningAdvanced, FinishReason: resp.FinishReason}
			if idx == 0 {
				step.Reply = turnReply // the turn's narration rides its first step (review #6).
			}
			usageForTurn, modelMsForTurn = llm.Usage{}, 0

			toolStart := time.Now()
			sig := c.Name + " " + signatureArg(c, cfg.Tools)
			readOnly := c.Name == "read_file" || c.Name == "search" || c.Name == "list_dir"
			var obs string
			var isErr bool
			if readOnly && sig == lastReadSig && sig == lastExecutedSig && !reasoningAdvanced {
				obs = "(skipped: identical to your previous read; result unchanged) " + lastReadObs
				isErr = false
				step.ToolMs = time.Since(toolStart).Milliseconds()
			} else {
				if idx < prefetchK {
					obs, isErr = prefetched[idx].obs, prefetched[idx].isErr
					step.ToolMs = prefetched[idx].dur.Milliseconds()
				} else {
					obs, isErr = dispatchNative(loopCtx, cfg.Tools, c)
					step.ToolMs = time.Since(toolStart).Milliseconds()
				}
				lastExecutedSig = sig
				if readOnly && !isErr {
					lastReadSig = sig
					lastReadObs = obs
				}
			}
			if !isErr {
				grounded = true // (P4) the model has now seen real external state.
			}
			if isMutatingTool(c.Name) && !isErr {
				ev.mutation()
			}

			// (P5) Stagnant-observation + churn tracking (shared tracker), evaluated
			// on `run` results. Done BEFORE feeding the observation back so the churn
			// nudge can ride the very message the model reads. A `run` that keeps
			// failing with the byte-identical result, across turns whose ACTIONS
			// differ, is a stall the turn-signature/spiral detectors can't see
			// (DOGFOOD R9/R10's 30-turn edit→test→edit→same-error burn).
			kill := false
			stagnantCount := 0
			if c.Name == "run" {
				kill, stagnantCount = tr.observeRun(obs)
				recordNativeRun(loopCtx, cfg, standing, tr, c, obs)
			}
			if tr.observeAction(i, c.Name) {
				editedThisTurn = true
			}
			// Build the observation-repeat fingerprint from the RAW tool output only.
			// Hints/nudges are still appended to what the model reads below, but they
			// must never perturb the hard repeat detector's key: escalating hints differ
			// at counts 3/4/5, and baking them in would reset the counter before the
			// count-6 KilledRepeat backstop can fire.
			rawObs := obs
			turnObservationFPs = append(turnObservationFPs, c.Name+" "+signatureArg(c, cfg.Tools)+"\nOBSERVATION:\n"+rawObs)

			// (P3) Churn nudge. Skipped on a kill turn (we're terminating anyway).
			if !kill && tr.churnNudge() {
				obs += churnNudge
			}

			// Green-repeat nudge: the model keeps re-running the same passing
			// command with no file changes between — nudge, never kill.
			if !kill && tr.greenRepeatNudge() {
				obs += greenRepeatNudgeText
			}

			// (Lever 2b) Wire dedup-at-source. rawObs already fed the repeat-detector
			// fingerprint above, so the count-6 KilledRepeat and the 3/4/5 escalating
			// nudges are unaffected; this shrinks ONLY the billed copy. If these exact
			// observation bytes were sent at an earlier iteration, replace the raw portion
			// with a one-line stub, preserving any nudge suffix appended above. Append-only
			// ⇒ prefix cache stays valid. `run` still executes (execution-dedup is a
			// separate, consecutive-read-only mechanism); this only dedupes bytes.
			if s, dup := dd.stub(rawObs, i); dup {
				obs = s + strings.TrimPrefix(obs, rawObs)
			}

			step.Grounded = grounded
			step.Observation = obs
			res.Steps = append(res.Steps, step)
			cfg.Obs.Observation(obs)
			// (P6) A tool failure is an observation the model reacts to, tagged so
			// the provider marks it an error result — never a crash for us.
			messages = append(messages, llm.ToolResultMsg(c.ID, obs, isErr))

			if kill {
				// (finding #1) The kill fires mid-turn, so any calls after this one are
				// never dispatched — pair each with a synthetic error result so the
				// assistant turn we appended above has no unanswered tool call in the
				// returned transcript (the continuation seam).
				for _, rc := range calls[idx+1:] {
					messages = append(messages, llm.ToolResultMsg(rc.ID, "not executed: run stopped by the no-progress detector", true))
				}
				res.Outcome = KilledStagnant
				res.Reason = fmt.Sprintf("no progress: the same command failure recurred %d times despite changing actions — the approach is stuck; change strategy or rewrite the file", stagnantCount)
				return gs.upgradeIfVerified(ctx, res), nil
			}
		}

		// Observation-repeat hard stop: if the same fully observed turn (tool call
		// signature plus the observation text returned to the model) recurs at the
		// hard reasoning ceiling, reasoning-signature movement alone is not progress.
		// This is after all tool results were appended, so returned native transcripts
		// stay well-formed; the pre-dispatch action-only detector still owns the
		// earlier no-reasoning KilledRepeat path and message shape.
		if kill, count := tr.observeToolObservation(strings.Join(turnObservationFPs, "\n---CALL---\n")); kill {
			res.Outcome = KilledRepeat
			res.Reason = fmt.Sprintf("no progress: repeated %q %d times", sig, count)
			return gs.upgradeIfVerified(ctx, res), nil
		} else if msg := escalatingRepeatNudge(count); msg != "" {
			messages = append(messages, llm.User(msg))
		}

		// (code-intel slice 1) Diagnostics feed (shared tracker), once per turn
		// after the tool results are in the transcript (so the conversation stays
		// well-formed, like the finisher below), and only when the model edited
		// this turn. Surfaced as a standalone user message (the native loop's wire
		// format), INFORMATION not a gate (see docs/specs/CODE-INTELLIGENCE.md).
		if editedThisTurn {
			if msg := tr.diagnostics(loopCtx, runTimeout); msg != "" {
				messages = append(messages, llm.User(msg))
			}
		}

		// (HP-4) Near-cap finisher (shared tracker), evaluated once per TURN
		// (after all of this turn's tool results are in the transcript, so the
		// conversation stays well-formed): a standalone hint the model reads next
		// turn. See Config.FinishNudgeWindow.
		if tr.finishNudge(i) {
			cfg.Obs.Note("near cap with a green build and stable files — nudging to finish (HP-4)")
			messages = append(messages, llm.User(finishNudgeNative))
		}

		// Unconditional near-cap answer-forcer for an observe-only agent (no build
		// signal to gate on). Fires at most once, leaving a turn to act. See
		// Config.AnswerNudgeWindow (council code critic).
		if !answerNudged && cfg.AnswerNudgeWindow > 0 && answerNudgeOK && i < maxIter &&
			maxIter-i <= cfg.AnswerNudgeWindow {
			cfg.Obs.Note("near cap — nudging the agent to stop exploring and answer")
			messages = append(messages, llm.User(answerNudgeNative))
			answerNudged = true
		}

		// (DUET-DOGFOOD F2) Near-cap reminder for a finish-tool caller: the work an
		// agent does survives in its files, but an unsent message is LOST on hit_cap
		// (the 2026-06-12 validation duet, turn 7: the model finished and tested its
		// file at i14–16, then the cap fell before it ever called `say`). Unlike the
		// HP-4 finisher this needs no green-build gate — calling the finish tool
		// can't mask broken state, it just delivers the message the partner is
		// waiting for. Fires once, leaving a turn to act.
		if !sayNudged && cfg.FinishTool != "" && i < maxIter && maxIter-i <= finishToolNudgeWindow {
			cfg.Obs.Note("near cap — reminding the agent to finish with the " + cfg.FinishTool + " tool")
			messages = append(messages, llm.User(fmt.Sprintf(
				"[hint: your action budget is nearly spent (%d turn(s) left). Wrap up NOW: call the %q tool with a short message — your files are saved either way, but a message you never send is lost.]",
				maxIter-i, cfg.FinishTool)))
			sayNudged = true
		}
	}

	res.Outcome = HitCap
	res.Reason = fmt.Sprintf("hit iteration cap (%d) without an answer", maxIter)
	return gs.upgradeIfVerified(ctx, res), nil
}

// observeOnlyTools is the allowlist of built-in tools that cannot mutate state or
// execute code. isObserveOnly checks membership against it rather than denylisting
// known effect tools, so it FAILS CLOSED: a custom or future effectful tool the
// harness doesn't know about is NOT observe-only (dogfood slice 4, round 3, O3).
var observeOnlyTools = map[string]bool{"list_dir": true, "read_file": true, "search": true, "go_doc": true}

// isObserveOnly reports whether EVERY tool in the set is a known read-only built-in.
// An agent so equipped can't leave work half-done, so the near-cap answer-forcer is
// safe to fire for it (see Config.AnswerNudgeWindow). Empty set => true (no effects).
func isObserveOnly(tools map[string]Tool) bool {
	for name := range tools {
		if !observeOnlyTools[name] {
			return false
		}
	}
	return true
}

func recordNativeRun(ctx context.Context, cfg Config, standing *standingState, tr *turnTracker, c llm.ToolCallPart, obs string) {
	if tr == nil {
		return
	}
	cmd := runCommandArg(c, cfg.Tools)
	tree := ""
	if cfg.StandingContext && standing != nil {
		if cur, err := standing.currentTree(ctx, cfg); err == nil {
			tree = cur
		}
	}
	tr.recordRun(cmd, obs, tree)
}

func runCommandArg(c llm.ToolCallPart, tools map[string]Tool) string {
	if t, ok := tools[c.Name]; ok && t.RunJSON != nil {
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(c.Args, &a) == nil {
			return strings.TrimSpace(a.Command)
		}
	}
	return strings.TrimSpace(bridgeArg(c))
}

// dispatchNative runs the tool a call names and turns any failure into an error
// observation (P6). A tool with a structured RunJSON gets the model's typed JSON
// args handed straight through — no in-string parsing. A tool with only Run is
// bridged: we pull its single "arg" field and call Run unchanged, so custom
// toolsets keep working in native mode.
func dispatchNative(ctx context.Context, tools map[string]Tool, c llm.ToolCallPart) (observation string, isErr bool) {
	t, ok := tools[c.Name]
	if !ok {
		return fmt.Sprintf("ERROR: unknown tool %q — available: %s", c.Name, strings.Join(toolNames(tools), ", ")), true
	}
	var (
		out string
		err error
	)
	if t.RunJSON != nil {
		out, err = t.RunJSON(ctx, c.Args)
	} else {
		out, err = t.Run(ctx, bridgeArg(c))
	}
	if err != nil {
		return "ERROR: " + err.Error(), true
	}
	return truncate(out), false
}

// toolCalls extracts the ToolCallParts from a response's content (ignoring any
// accompanying text/other parts).
func toolCalls(parts []llm.ContentPart) []llm.ToolCallPart {
	var out []llm.ToolCallPart
	for _, p := range parts {
		if tc, ok := p.(llm.ToolCallPart); ok {
			out = append(out, tc)
		}
	}
	return out
}

// callArg returns the deterministic string that represents a call's arguments
// for the trace (Step.Arg), the live summary, and the no-progress detectors. A
// structured tool (RunJSON set) has multi-field args, so its signature is the
// COMPACT JSON of those fields (stable for dedupe); a bridge tool's is its single
// "arg" string. Keying the detectors on this means "same path + same range twice"
// still trips the repeat detector under structured args, exactly as it did under
// the one-line text arg.
func callArg(c llm.ToolCallPart, tools map[string]Tool) string {
	if t, ok := tools[c.Name]; ok && t.RunJSON != nil {
		return compactJSON(c.Args)
	}
	return bridgeArg(c)
}

// findCall returns the first call whose tool name matches, or nil. Used to spot a
// FinishTool call in a turn that may also carry other (side-effecting) calls.
func findCall(calls []llm.ToolCallPart, name string) *llm.ToolCallPart {
	for i := range calls {
		if calls[i].Name == name {
			return &calls[i]
		}
	}
	return nil
}

// finishToolMessage extracts the human-facing message a FinishTool call carries.
// The canonical schema is {"message": "..."}; it falls back to the bridge "arg"
// field so a tool registered without the structured schema still yields its text.
func finishToolMessage(c llm.ToolCallPart) string {
	var a struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(c.Args, &a) == nil {
		if m := strings.TrimSpace(a.Message); m != "" {
			return m
		}
	}
	return strings.TrimSpace(bridgeArg(c))
}

// bridgeArg pulls the bridge schema's single "arg" string from a call's JSON
// args. A malformed/edge args object yields "" — the tools already treat ""
// sensibly (e.g. list_dir "" = root), so a soft fallback beats erroring here.
func bridgeArg(c llm.ToolCallPart) string {
	var a struct {
		Arg string `json:"arg"`
	}
	if json.Unmarshal(c.Args, &a) == nil {
		return a.Arg
	}
	return ""
}

// compactJSON strips insignificant whitespace from raw JSON while preserving the
// model/provider's object key order. It is used for human-facing trace strings,
// where matching the emitted argument order is harmless and avoids changing
// transcript shape. Signature paths must use canonicalJSON instead.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
}

// canonicalJSON returns a stable, semantic JSON form for no-progress signatures.
// It decodes with UseNumber so numeric lexemes survive intact, then marshals the
// value back through encoding/json; maps are emitted with sorted keys recursively.
// Signature helpers are deliberately best-effort: malformed or otherwise
// unmarshalable JSON falls back to compactJSON rather than making detector code
// capable of failing a turn.
func canonicalJSON(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return compactJSON(raw)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return compactJSON(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return compactJSON(raw)
	}
	return string(b)
}

func signatureArg(c llm.ToolCallPart, tools map[string]Tool) string {
	if t, ok := tools[c.Name]; ok && t.RunJSON != nil {
		return canonicalJSON(c.Args)
	}
	return bridgeArg(c)
}

// reasoningSignature fingerprints a turn's opaque reasoning trace (llm.ReasoningPart,
// e.g. OpenRouter `reasoning_details`) so the tight-loop detector can distinguish
// "same action, NEW thought" (a thinking model still working) from "same action,
// same thought" (a genuine stall). It is empty when the model returned no reasoning,
// which makes the repeat detector behave identically to the action-only version for
// non-reasoning providers. The raw bytes are used verbatim — the content is opaque,
// so any change at all counts as the reasoning having moved.
//
// Consequence (review #12): a provider that re-encrypts the trace every call
// (Gemini via OpenRouter) "advances" every turn by construction, so such models
// always run under the LENIENT detector thresholds — see the maxReasoningRepeats
// comment in agent.go for why that stands and what the escalation path is.
func reasoningSignature(parts []llm.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if rp, ok := p.(llm.ReasoningPart); ok {
			b.Write(rp.Raw)
		}
	}
	return b.String()
}

// turnSignature is the deterministic per-turn dedupe key for the no-progress
// detectors: each call's name + its arg signature, in order. Two turns the model
// re-issues identically share a signature; a parallel fan-out is ONE signature,
// not several — so the detector keys on across-turn stagnation, never on the
// breadth of a single turn.
func turnSignature(calls []llm.ToolCallPart, tools map[string]Tool) string {
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = c.Name + " " + signatureArg(c, tools)
	}
	return strings.Join(parts, " | ")
}

// discoveryTools are the pure-DISCOVERY built-ins: they return POINTERS — names
// (list_dir) or matching-line locations (search) — rather than committing to a
// target's content (read_file) or changing state (edit_file/write_file/run). A run
// of discovery-only turns is the generalized explore-spiral: the model keeps
// gathering pointers and never follows one (list_dir a, search x, list_dir b …).
// read_file is deliberately EXCLUDED: paging/reads consume content (progress), a
// re-read of the same arg is the reasoning-aware repeat detector's job, and folding
// reads into churn would reintroduce the thinking-model false-kill (HARD-PROBLEMS.md
// HP-2; see docs/findings/HP2-TEMPLATE-COLLAPSE.md for why the heavier working-set design collapses
// to exactly this).
var discoveryTools = map[string]bool{"list_dir": true, "search": true}

// allCallsDiscovery reports whether EVERY call in a turn is a discovery tool — a
// pure-discovery turn. It generalizes the old list_dir-only check to the discovery
// CLASS, so search-churn (search x, search y, search z …) and mixed list_dir/search
// wandering trip the same spiral the list_dir-only case did. A turn that reads,
// edits, or runs is NOT discovery-only, so it breaks the run — search-then-read
// reconnaissance is progress and resets the counter. A parallel fan-out of several
// discovery calls in ONE turn is a single discovery turn, not several.
func allCallsDiscovery(calls []llm.ToolCallPart) bool {
	for _, c := range calls {
		if !discoveryTools[c.Name] {
			return false
		}
	}
	return len(calls) > 0
}

// discoveryTargets returns the NORMALIZED frontier target of each discovery call
// in a turn. The frontier-aware spiral detector (spiralState) tells orientation
// (a NEW target) from a cycle (all targets already visited), so the key must be
// the SEMANTIC target — the directory being listed or the query being run — not
// the raw argument spelling. Equivalent list_dir paths ("a", "./a", "a/", "a/.")
// name one directory and must share one key, else a true navigation cycle over
// re-spelled paths dodges the cycle window. Callers pass only discovery turns.
func discoveryTargets(calls []llm.ToolCallPart, tools map[string]Tool) []string {
	targets := make([]string, len(calls))
	for i, c := range calls {
		targets[i] = discoveryTargetForCall(c, tools)
	}
	return targets
}

// discoveryTargetForCall builds the normalized frontier key for one native
// discovery call, reading the typed path/pattern fields (falling back to the
// bridge "arg" for tool-less providers) so path-spelling variants collapse.
func discoveryTargetForCall(c llm.ToolCallPart, tools map[string]Tool) string {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Arg     string `json:"arg"`
	}
	_ = json.Unmarshal(c.Args, &a)
	switch c.Name {
	case "list_dir":
		p := a.Path
		if p == "" {
			p = a.Arg // bridge/tool-less form carries the path as the single "arg".
		}
		return "list_dir " + normalizeDirPath(p)
	case "search":
		pat := a.Pattern
		if pat == "" {
			pat = a.Arg
		}
		key := "search " + strings.TrimSpace(pat)
		// A structured search may scope to a subtree; fold the normalized scope in
		// so "search x @ a" and "search x @ ./a" collapse but a whole-repo "search x"
		// stays distinct from a scoped one.
		if a.Pattern != "" && strings.TrimSpace(a.Path) != "" {
			key += " @ " + normalizeDirPath(a.Path)
		}
		return key
	default:
		return c.Name + " " + signatureArg(c, tools)
	}
}

// textDiscoveryTarget builds the normalized frontier key for one text-protocol
// discovery call, whose argument is a single string: the list_dir path or the
// search query. It mirrors discoveryTargetForCall so both loops key on the same
// semantic target (deliverable 3 — the two loops must not drift).
func textDiscoveryTarget(verb, arg string) string {
	if verb == "list_dir" {
		return "list_dir " + normalizeDirPath(arg)
	}
	return verb + " " + strings.TrimSpace(arg)
}

// normalizeDirPath collapses equivalent spellings of a directory path to one
// canonical form (path.Clean), so "a", "./a", "a/", and "a/." all key as "a".
// An empty or whitespace-only path is the repository root (".").
func normalizeDirPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

// turnSteps builds the trace steps for a turn killed by a detector BEFORE
// dispatch (so they carry no observation): one Step per call, the turn's usage
// attributed to the first, all flagged with the run's current grounded state.
func turnSteps(iter int, calls []llm.ToolCallPart, tools map[string]Tool, grounded bool, usage llm.Usage, modelMs int64, reasoningAdvanced bool, reply string, finish llm.FinishReason) []Step {
	steps := make([]Step, len(calls))
	for i, c := range calls {
		// The turn's single model call produced ALL its calls — attribute its usage
		// and latency to the first step only (a per-turn cost, not per-call), like the
		// live tool loop does; the model's prose narration (reply) rides the first
		// step the same way (review #6). ReasoningAdvanced and FinishReason are turn
		// properties, so every step carries them.
		u, mm, rep := llm.Usage{}, int64(0), ""
		if i == 0 {
			u, mm, rep = usage, modelMs, reply
		}
		steps[i] = Step{Iter: iter, Verb: c.Name, Arg: callArg(c, tools), Grounded: grounded, Usage: u, ModelMs: mm, Reply: rep, ReasoningAdvanced: reasoningAdvanced, FinishReason: finish}
	}
	return steps
}

// callsSummary renders a turn's tool calls for the live observer (one-line, like
// the text loop's "model:" line).
func callsSummary(calls []llm.ToolCallPart, tools map[string]Tool) string {
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = c.Name + " " + oneLine(callArg(c, tools))
	}
	return strings.Join(parts, " | ")
}

// nativeSchemas advertises each tool to the model: its hand-written structured
// Schema when present (typed multi-field args, zero in-string parsing), else a
// single-string "arg" bridge schema that reuses the tool's Run unchanged. The
// bridge is the backward-compat path for custom toolsets that only set Run —
// and because the arg arrives as a JSON string, even bridged multi-line content
// is escape-free.
func nativeSchemas(tools map[string]Tool) []llm.Tool {
	out := make([]llm.Tool, 0, len(tools))
	for _, name := range toolNames(tools) {
		t := tools[name]
		// Prefer the behavior-only NativeDesc; Desc carries text-protocol framing
		// (one-line ARG grammar, \n escapes) that is FALSE and misleading in native
		// mode, where the per-field Schema descriptions own the format.
		desc := t.Desc
		if t.NativeDesc != "" {
			desc = t.NativeDesc
		}
		schema := t.Schema
		if len(schema) == 0 {
			// Bridge fallback for a Run-only custom tool: a single-string `arg`.
			// Its description still comes from `desc` (NativeDesc if the tool set one).
			schema, _ = json.Marshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"arg": map[string]any{"type": "string", "description": desc},
				},
				"required": []string{"arg"},
			})
		}
		out = append(out, llm.Tool{Name: t.Name, Description: desc, Schema: schema})
	}
	return out
}

// nativeSystemPrompt is the role + termination contract. The tools themselves are
// advertised through the API (name + schema), so unlike the text protocol this
// prompt does NOT enumerate a line grammar — it only states how to finish (P5).
func nativeSystemPrompt() string {
	return "You are a tool-using agent. Use the provided tools to accomplish the TASK, " +
		"basing each action on the latest tool results (real external state). " +
		"Pick the tool that most directly advances the task. " +
		"When you have enough information to finish, reply with your final answer as plain text and DO NOT call a tool."
}

// structuredSystemPrompt is the PROMPT-SKILLS slice-2 candidate: the same loop
// contract as nativeSystemPrompt plus a short block of working rules. The rules
// are the council-calibrated forms (docs/specs/PROMPT-SKILLS.md, run
// 20260703-104826-a38361): scoped verification with a feasibility escape hatch
// (an explanation-only task must not trigger a test run), a test-integrity rule
// that blocks reward hacking WITHOUT blocking test maintenance, and the
// convention/dependency/comment/scope discipline every serious harness ships.
// Deliberately SHORT (~250 words): cheap models degrade under long prompts and
// attend to few instructions, so every rule here has to earn its slot — depth
// lives in the tool descriptions and skills, which load per-use. The blunt
// IMPORTANT/NEVER emphasis is intentional for the cheap-model tier this profile
// targets; do not "clean it up" into calm prose without re-running the A/B.
func structuredSystemPrompt() string {
	return `You are a software engineering agent working in a sandboxed project checkout. Use the provided tools to accomplish the TASK, basing each action on the latest tool results (real external state). Pick the action that most directly advances the task.

Working rules:
- VERIFY before you finish: run the most relevant available check (the task's named test/build command, or the tests for the package you changed — not the whole suite when a narrower run answers it). If no check is feasible (no tests, broken dependencies, or an explanation-only task where running code proves nothing), say exactly why and what weaker verification you did. NEVER present unverified work as verified.
- Tests are the spec: NEVER weaken, delete, or special-case a test just to make it pass. Adding a regression test, updating a golden file, or changing a test because the TASK intentionally changed behavior is normal work — do it and state the reason.
- Follow the existing code: mimic the surrounding style, naming, and idioms; use the project's own utilities. NEVER assume a library is available — confirm it is already a dependency (imports, go.mod, or the language's manifest) before using it.
- Comments: do not narrate your changes or restate what code does. Comment only a non-obvious WHY, matching the file's existing comment density.
- Scope: do what the TASK asks, nothing more — no drive-by refactors, renames, or extra files. If you checked and something genuinely isn't there, saying so is a valid answer.

When the TASK is complete and verified, reply with your final answer as plain text and DO NOT call a tool.`
}

// Profile-to-prompt resolution lives in prompt_family.go (resolveSystemPrompt):
// slice 2 added the fixed legacy/structured profiles, slice 3 the model-family
// "auto" routing.
