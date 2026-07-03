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
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// RunNative executes the agent against a tool-capable provider. Its signature and
// RunResult match Run exactly, so a caller swaps loops without other changes.
func RunNative(ctx context.Context, cfg Config) (out *RunResult, err error) {
	// (P1 spine) Same run-identity stamping as Run, registered BEFORE the isolation
	// refusal so a refused run is addressable too (its transcript write won't fail
	// on an empty ID).
	runID, startedAt := newRunID(), time.Now()
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
		return refusal, nil // (P2/§5) too-weak sandbox — refuse before the first model call.
	}
	// (PROMPT-SKILLS slices 2+3) Resolve the base prompt BEFORE any paid call
	// (the plan stage bills first) — an unknown profile must abort, not run
	// mislabeled. The routing note (profile "auto") is surfaced once Obs exists:
	// the family decision must be visible in every transcript, never silent.
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
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}
	maxTok := cfg.MaxTokens
	if maxTok <= 0 {
		maxTok = DefaultMaxTokens
	}
	spiralWindow := cfg.NavSpiralWindow
	if spiralWindow <= 0 {
		spiralWindow = noProgressWindow
	}
	runTimeout := cfg.RunTimeout
	if runTimeout <= 0 {
		runTimeout = defaultRunTimeout
	}
	if cfg.Tools == nil {
		cfg.Tools = DefaultTools(cfg.Sandbox, runTimeout)
	}
	// (REVIEW-GATE slice 0) Fence the mutation tools BEFORE their schemas are
	// advertised, and snapshot the closing gates' run-start state (fence hashes
	// + the review diff base). Inert when unconfigured — see Run's twin.
	// Diff-scope wraps FIRST, test-fence LAST, so the fence wins for fenced
	// in-scope paths.
	cfg.Tools = applyDiffScope(cfg.Tools, cfg.DiffScope, cfg.Sandbox)
	cfg.Tools = applyTestFence(cfg.Tools, cfg.TestFence, cfg.Sandbox)
	gs := newGates(ctx, cfg, runTimeout)

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
	gs.applyBaseline(res)
	// AbortOnRedBaseline: if the baseline is red and the caller wants us to stop,
	// return immediately before any model call — the gate is unsatisfiable at base.
	if cfg.AbortOnRedBaseline && gs.verifyBaselineRed {
		res.Outcome = Unverified
		res.Reason = fmt.Sprintf(
			"refused to run: the verify command %q is ALREADY failing on the untouched workspace — "+
				"the gate is unsatisfiable at base (-verify-baseline=abort caused this refusal)",
			cfg.VerifyCmd,
		)
		res.Iterations = 0
		return res, nil
	}
	// The review report travels on EVERY exit path (findings + fates are the
	// calibration telemetry) — nil when the gate is off.
	defer func() {
		if out != nil {
			out.Review = gs.reviewReport()
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
	messages := seedMessages(seedCfg, observeEnvironment(ctx, cfg.Sandbox)+gs.baselinePreamble())
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
	temp := 0.0

	// (P5) No-progress state. Unlike the text loop these are evaluated PER TURN, not
	// per call: a native turn can carry several tool calls at once (parallel
	// fan-out), so a call-by-call detector would mistake one 4-way list_dir turn for
	// a 4-turn spiral. lastTurnSig dedupes whole turns; navRun counts consecutive
	// list_dir-only turns.
	var lastTurnSig string
	var lastReasoningSig string // (P5) the prior turn's reasoning fingerprint — see the repeat detector.
	repeats, navRun := 0, 0
	grounded := false // (P4) gates memory writes — only a verified answer is stored.

	// (review #9) The stagnant/churn/diagnostics/finisher state shared with Run
	// lives in ONE tracker; the vars above stay loop-local because they are
	// wire-format-specific (per-turn here, per-call in the text loop). The
	// stagnant detector is still evaluated on each `run` result regardless of
	// which turn it lands in.
	tr := newTurnTracker(cfg, maxIter)
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

	for i := 1; i <= maxIter; i++ {
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
		cfg.Obs.Iteration(i, maxIter)

		// generateWithEviction adds HP-1's reactive fallback: on a window overflow it
		// compacts the OLDEST turn (pairing-safe) and retries instead of crashing,
		// returning the possibly-shrunk transcript we carry forward.
		var resp *llm.Response
		var err error
		modelStart := time.Now()
		resp, messages, err = generateWithEviction(loopCtx, cfg, llm.Request{
			System:          system,
			Messages:        messages,
			Tools:           schemas,
			Temperature:     &temp,
			MaxTokens:       maxTok,
			ReasoningEffort: cfg.ReasoningEffort,
		})
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
			// A deadline hit mid-Generate is the wall-clock budget, not a transport fault.
			if cfg.MaxWallClock > 0 && loopCtx.Err() == context.DeadlineExceeded {
				res.Outcome = HitDeadline
				res.Reason = fmt.Sprintf("hit wall-clock budget (%s) mid-turn", cfg.MaxWallClock)
				return gs.upgradeIfVerified(ctx, res), nil
			}
			// (HP-1) Window overflowed and eviction couldn't compact it further —
			// degrade gracefully rather than mislabel it a transport fault.
			if errors.Is(err, llm.ErrContextLength) {
				res.Outcome = HitContextLimit
				res.Reason = "context window exceeded and could not be compacted further"
				return gs.upgradeIfVerified(ctx, res), nil
			}
			// A caller cancel is a normal typed stop, not an infrastructure error.
			// Check the PARENT ctx — loopCtx may also carry DeadlineExceeded from
			// MaxWallClock, and wall-clock expiry must read HitDeadline, not Canceled.
			// We check ctx.Err() because signal.NotifyContext (Go 1.26+) cancels with
			// a custom signalError cause, and Err() is cause-agnostic while still
			// distinguishing deadline expiry.
			if errors.Is(ctx.Err(), context.Canceled) {
				res.Outcome = Canceled
				res.Reason = "run canceled by the caller (interrupt)"
				return res, nil
			}
			res.Outcome, res.Reason, res.Err = ProviderErr, err.Error(), err
			return res, err
		}
		res.Iterations = i
		res.Usage = addUsage(res.Usage, resp.Usage)
		noteUsage(cfg.Obs, i, res.Usage, cfg.MaxTotalTokens)

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
			// return so RunResult.Messages ends with the answer the model gave. Covers all
			// three exits below (Unverified-on-fail, empty-answer, Answered); the
			// VerifyContinue branch relied on its own append before this existed.
			messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})
			// (P5/HP-5) A tool-call-free turn is the done-signal — but it fires even
			// when the model narrated intent, acknowledged failure, or hallucinated
			// success (DOGFOOD R9/R10, the most common false-positive in the bake-offs).
			// Re-verify the claimed state before accepting it (fence first, then VerifyCmd).
			outcome, reason, noContinue := gs.verifyTermination(ctx, tr.lastRunFailed)
			// ScopeViolation is terminal — never continue, regardless of VerifyContinue.
			if outcome == ScopeViolation {
				res.Outcome = ScopeViolation
				res.Reason = reason
				return res, nil
			}
			// A caller cancel mid-answer stops the run cleanly as Canceled — the
			// verify command was skipped (verifyRun refuses on a canceled ctx), and
			// the run must not continue (the next model call would also fail).
			// We check ctx.Err() because signal.NotifyContext (Go 1.26+) cancels with
			// a custom signalError cause, and Err() is cause-agnostic while still
			// distinguishing deadline expiry.
			if errors.Is(ctx.Err(), context.Canceled) {
				res.Outcome = Canceled
				res.Reason = "run canceled by the caller (interrupt)"
				return res, nil
			}
			if reason != "" && cfg.VerifyContinue && i < maxIter && !noContinue {
				// Continue-on-fail: re-ground with the real failing state (P4) and keep
				// working rather than accept a premature finish. The assistant's
				// tool-call-free turn must precede the feedback so the conversation stays
				// well-formed.
				cfg.Obs.Note("finish rejected (not verified) — continuing")
				// (the assistant's tool-call-free turn was appended above, before the
				// res.Steps record — the conversation stays well-formed for the feedback.)
				messages = append(messages, llm.User("OBSERVATION:\nNot finished — you stopped calling tools, but the task is not verified:\n"+reason+"\nKeep working: fix the code and re-run until it passes."))
				continue
			}
			if reason != "" {
				res.Outcome, res.Answer, res.Reason = outcome, answer, reason
				cfg.Obs.Note("answer not verified — " + reason)
				return res, nil
			}
			// (Empty-answer guard) The model ended its turn with no tool call AND no
			// prose. Verification may have passed (or none was configured), but a
			// content-free finish is the model going silent, not a delivered answer —
			// recording it as Answered/exit-0 lets an empty string read as a clean pass.
			// Flag it as a non-pass instead; the defer still salvages any earlier prose
			// into Answer for a relaying caller, but the Outcome stays Unverified.
			if answer == "" {
				res.Outcome, res.Reason = Unverified, "empty final answer — the model stopped without producing an answer"
				cfg.Obs.Note("empty final answer — recording as unverified, not a clean pass")
				return res, nil
			}
			// (REVIEW-GATE slice 1) Stage 2, only now that fence + VerifyCmd are
			// green (execution-first) — see Run's twin for the contract.
			if fb, blockReason := gs.reviewFinish(ctx, i < maxIter); fb != "" {
				cfg.Obs.Note("finish rejected (review blockers) — continuing")
				messages = append(messages, llm.User("OBSERVATION:\n"+fb))
				continue
			} else if blockReason != "" {
				res.Outcome, res.Answer, res.Reason = Unverified, answer, blockReason
				cfg.Obs.Note("answer blocked by review — " + blockReason)
				return res, nil
			}
			res.Outcome, res.Answer = Answered, answer
			cfg.Obs.Done(answer)
			if grounded {
				res.memDone = rememberAsync(ctx, cfg.Obs, cfg.Memory, scope, cfg.Task, answer)
			} else if cfg.Memory != nil {
				cfg.Obs.Note("memory: answer not tool-verified this run — not stored (avoids amplifying guessed/recalled facts)")
			}
			return res, nil
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
					if !isErr {
						grounded = true
					}
					if c.Name == "run" {
						tr.observeRun(obs) // so a VerifyLastRun gate reads this turn's last run (kill ignored — we're finishing).
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
				// (FinishTool verification, finding #3) An explicit finish is NOT ground
				// truth that the task is done: unless the caller vouches for it
				// (FinishToolTrustsCaller), route it through the SAME authoritative gate
				// as prose termination. For a caller with no VerifyCmd/VerifyLastRun
				// (duet's `say`) verifyTermination is a no-op, so the conversational
				// finish path is unchanged.
				if !cfg.FinishToolTrustsCaller {
					outcome, reason, noContinue := gs.verifyTermination(ctx, tr.lastRunFailed)
					// ScopeViolation is terminal — never continue.
					if outcome == ScopeViolation {
						res.Outcome = ScopeViolation
						res.Reason = reason
						return res, nil
					}
					// A caller cancel mid-finish stops the run cleanly as Canceled.
					// We check ctx.Err() because signal.NotifyContext (Go 1.26+) cancels with
					// a custom signalError cause, and Err() is cause-agnostic while still
					// distinguishing deadline expiry.
					if errors.Is(ctx.Err(), context.Canceled) {
						res.Outcome = Canceled
						res.Reason = "run canceled by the caller (interrupt)"
						return res, nil
					}
					if reason != "" {
						if cfg.VerifyContinue && i < maxIter && !noContinue {
							cfg.Obs.Note("finish rejected (not verified) — continuing")
							messages = append(messages, llm.User("OBSERVATION:\nNot finished — you called the finish tool, but the task is not verified:\n"+reason+"\nKeep working: fix the code and re-run until it passes."))
							continue
						}
						res.Outcome, res.Answer, res.Reason = outcome, msg, reason
						cfg.Obs.Note("finish not verified — " + reason)
						return res, nil
					}
					// (REVIEW-GATE slice 1) Stage 2 gates the explicit finish exactly
					// like prose termination. A trusted-caller finish (duet's `say`)
					// skips it with the rest of the gate, above.
					if fb, blockReason := gs.reviewFinish(ctx, i < maxIter); fb != "" {
						cfg.Obs.Note("finish rejected (review blockers) — continuing")
						messages = append(messages, llm.User("OBSERVATION:\n"+fb))
						continue
					} else if blockReason != "" {
						res.Outcome, res.Answer, res.Reason = Unverified, msg, blockReason
						cfg.Obs.Note("finish blocked by review — " + blockReason)
						return res, nil
					}
				}
				res.Outcome, res.Answer = Answered, msg
				cfg.Obs.Done(msg)
				if grounded {
					res.memDone = rememberAsync(ctx, cfg.Obs, cfg.Memory, scope, cfg.Task, msg)
				} else if cfg.Memory != nil {
					cfg.Obs.Note("memory: answer not tool-verified this run — not stored (avoids amplifying guessed/recalled facts)")
				}
				return res, nil
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
				res.Steps = append(res.Steps, turnSteps(i, calls, cfg.Tools, grounded, resp.Usage, modelMs, reasoningAdvanced, turnReply, resp.FinishReason)...)
				res.Outcome = KilledRepeat
				res.Reason = fmt.Sprintf("no progress: repeated %q %d times", sig, repeats)
				return gs.upgradeIfVerified(ctx, res), nil
			}
		} else {
			repeats = 0
		}
		lastTurnSig = sig
		lastReasoningSig = reasoningSig

		if allCallsDiscovery(calls) { // (b)
			// Two thresholds, mirroring the repeat detector (a). Measured false-kill
			// (SWE-bench stride-30, 2026-06-12): glm-5 opens every task with a grounded
			// descend-the-tree orientation burst (list_dir . → pkg → pkg/sub → targeted
			// search) and was executed at exactly iter 4 on instances other models
			// solved 2/2 — 12 of its 19 spiral kills. A turn whose reasoning trace
			// MOVED gets the lenient ceiling (the model is visibly working through the
			// listings it just received); a frozen or absent trace keeps the strict one,
			// so the original ungrounded list_dir pathology (a non-reasoning model
			// ignoring its observations) still dies at the window. Either way the spiral
			// stays bounded well before the iteration cap.
			if navRun++; navRun >= spiralLimit(spiralWindow, reasoningAdvanced) {
				res.Steps = append(res.Steps, turnSteps(i, calls, cfg.Tools, grounded, resp.Usage, modelMs, reasoningAdvanced, turnReply, resp.FinishReason)...)
				res.Outcome = KilledSpiral
				res.Reason = fmt.Sprintf("no progress: %d discovery-only turns in a row (list_dir/search) — read or edit a specific target, or answer", navRun)
				return gs.upgradeIfVerified(ctx, res), nil
			}
		} else {
			navRun = 0
		}

		// (P1) Committed to dispatch — NOW the assistant turn (carrying its tool-call
		// parts) becomes state we re-send; the API requires the tool_calls to precede
		// their results. Deferred to here (past the no-progress detectors above) so a
		// detector kill returns a well-formed transcript with no dangling tool-call —
		// the continuation seam bug (harness review finding #1).
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})

		// ACT: run every requested call in order, appending one result per id (a
		// missing result for any call would make the next request malformed).
		usageForTurn := resp.Usage // attribute the turn's usage to its first step only.
		modelMsForTurn := modelMs  // likewise the model-call latency: a per-turn cost.
		editedThisTurn := false    // (code-intel slice 1) did any call this turn mutate a file?
		for idx, c := range calls {
			step := Step{Iter: i, Verb: c.Name, Arg: callArg(c, cfg.Tools), Usage: usageForTurn, ModelMs: modelMsForTurn, ReasoningAdvanced: reasoningAdvanced, FinishReason: resp.FinishReason}
			if idx == 0 {
				step.Reply = turnReply // the turn's narration rides its first step (review #6).
			}
			usageForTurn, modelMsForTurn = llm.Usage{}, 0

			toolStart := time.Now()
			obs, isErr := dispatchNative(loopCtx, cfg.Tools, c)
			step.ToolMs = time.Since(toolStart).Milliseconds()
			if !isErr {
				grounded = true // (P4) the model has now seen real external state.
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
			}
			if tr.observeAction(i, c.Name) {
				editedThisTurn = true
			}
			// (P3) Churn nudge. Skipped on a kill turn (we're terminating anyway).
			if !kill && tr.churnNudge() {
				obs += churnNudge
			}

			// Green-repeat nudge: the model keeps re-running the same passing
			// command with no file changes between — nudge, never kill.
			if !kill && tr.greenRepeatNudge() {
				obs += greenRepeatNudgeText
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

// compactJSON strips insignificant whitespace from raw JSON so two identical
// calls produce an identical signature regardless of the provider's formatting.
// It preserves key order (json.Compact doesn't reorder), which is fine: temp=0
// makes the model emit the same field order for the same call.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.String()
	}
	return string(raw)
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
		parts[i] = c.Name + " " + callArg(c, tools)
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
