package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Run is the entire agent. Notice it is tiny — the loop is trivial (P3); the
// interesting work is the context policy and the termination conditions. It
// prints nothing: events go to cfg.Obs, the terminal state to the returned
// RunResult. err is non-nil ONLY for a genuine infrastructure failure (the model
// call itself failed); a no-progress kill or a hit cap is a normal Outcome, not
// a Go error.
func Run(ctx context.Context, cfg Config) (out *RunResult, err error) {
	// Stamp the run identity + wall-clock bounds on whatever result we return, from
	// every exit path, without threading it through each one (P1 spine: a run is
	// addressable by ID). Registered BEFORE the isolation refusal so even a refused
	// run gets an ID — otherwise the default CLI transcript write fails on an empty ID.
	runID, startedAt := newRunID(), time.Now()
	defer func() { stampRun(out, runID, startedAt) }()
	if refusal := checkIsolation(cfg); refusal != nil {
		return refusal, nil // (P2/§5) too-weak sandbox — refuse before the first model call.
	}
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}

	resolveAutoVerify(ctx, &cfg)

	// Resolve the OUR-side knobs from cfg-or-default (P5/P7). Done once, up front,
	// so the loop body reads from locals and the defaults live in exactly one place.
	knobs := resolveKnobs(cfg)
	maxIter, maxTok, runTimeout, spiralWindow := knobs.maxIter, knobs.maxTok, knobs.runTimeout, knobs.spiralWindow
	cfg.Tools = wrapTools(cfg, runTimeout)
	gs := newGates(ctx, cfg, runTimeout)

	res := &RunResult{Task: cfg.Task, Root: cfg.Root}
	recordAutoVerifyResolution(res, cfg)
	gs.applyBaseline(res)
	if refusal := redBaselineRefusal(cfg, gs); refusal != nil {
		res.Outcome, res.Reason, res.Iterations = refusal.Outcome, refusal.Reason, refusal.Iterations
		return res, nil
	}
	// The review report travels on EVERY exit path (findings + fates are the
	// calibration telemetry, recorded from day one) — nil when the gate is off.
	defer func() {
		if out != nil {
			out.Review = gs.reviewReport()
		}
	}()

	// (TRIAD) The opening PLAN stage: an injected planner explores the tree
	// read-only and its plan rides into the seeded task. Runs AFTER newGates on
	// purpose — the reviewer judges the ORIGINAL task, not the plan-augmented
	// one; recall (below) and RunResult.Task also keep the original. Fails open.
	planTask, planRep := runPlanStage(ctx, cfg)
	res.Plan = planRep
	seedCfg := cfg
	seedCfg.Task = planTask

	// ---- Principle 1: STATE LIVES HERE, in our slice. The model holds nothing. ----
	// We rebuild and re-send this whole conversation on every single call. A
	// continuing chat seeds it with the prior turns (Config.History); see Session.
	messages := seedMessages(seedCfg, observeEnvironment(ctx, cfg.Sandbox, cfg.BootContext)+verifyGatePreamble(cfg)+gs.baselinePreamble())
	// Expose the final conversation on every loop exit (the continuation seam, see
	// RunResult.Messages). Registered after `messages` exists so the closure reads
	// its final value; the closure captures the variable, which the loop reassigns.
	defer func() {
		if out != nil {
			out.Messages = messages
		}
	}()

	// ---- Principle 3: context IS the state. Long-term memory from PAST runs
	// (mneme) is surfaced into the system prompt before we think. The model gets
	// what it learned before, but labelled as possibly-stale so it still verifies. ----
	scope := scopeOrDefault(cfg.MemoryScope)
	system := withPersona(cfg.Persona, buildSystemPrompt(cfg.Tools)) + recall(ctx, cfg.Obs, cfg.Memory, scope, cfg.Task)
	temp := 0.0 // deterministic-ish; this is our knob, not the model's (P7).

	var lastAction string
	var lastReadSig string     // (Change A) signature of the last executed read-only call.
	var lastReadObs string     // (Change A) observation from the last executed read-only call.
	var lastExecutedSig string // signature of the immediately previous executed tool call.
	dd := newObsDedup()
	repeats := 0
	sameVerb := 0
	// lastReasoning holds the previous turn's opaque reasoning trace (concatenated
	// ReasoningPart.Raw). A thinking model whose trace keeps changing while its
	// visible action repeats gets the lenient tight-loop threshold (maxReasoningRepeats);
	// a frozen or absent trace falls back to the strict maxRepeats. See the repeat detector.
	var lastReasoning string

	// (review #9) The stagnant/churn/diagnostics/finisher state shared with
	// RunNative lives in ONE tracker; the loop-local vars above stay local
	// because they are wire-format-specific (per-call here, per-turn there).
	tr := newTurnTracker(cfg, maxIter)

	// grounded becomes true once a tool returns a real (non-error) observation
	// this run. It gates what we persist: we only remember answers that were
	// VERIFIED against real external state this session (Principle 4). This breaks
	// the amplification loop — a wrong/hallucinated answer, or one given purely
	// from recalled memory without re-checking, is NOT written back as a durable
	// "fact". mneme now consolidates on write (it can UPDATE/DELETE a stale fact
	// when a later Add contradicts it, see SetupMemory), but that only fires on the
	// facts we DO store — so this gate is still the first line of defense: a guess
	// we never write can never be the thing consolidation later has to walk back.
	grounded := false

	start := time.Now() // (P5) wall-clock budget anchor; see MaxWallClock.
	// Deadline-bound context so a slow in-flight call is cancelled AT the budget,
	// not merely noticed at the next between-turn check.
	loopCtx := ctx
	if cfg.MaxWallClock > 0 {
		var cancel context.CancelFunc
		loopCtx, cancel = context.WithTimeout(ctx, cfg.MaxWallClock)
		defer cancel()
	}

	var costBudgetMissingNoted bool

	for i := 1; i <= maxIter; i++ { // (P5) the hard cap lives in the loop header.
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

		// THINK: send the FULL context (P1) and get back text. Pure function.
		// generateWithEviction adds HP-1's reactive fallback: on a window overflow
		// it compacts the OLDEST turn and retries instead of crashing, returning the
		// possibly-shrunk transcript we carry forward.
		var resp *llm.Response
		var err error
		modelStart := time.Now()
		resp, messages, err = generateWithEviction(loopCtx, cfg, llm.Request{
			System:          system,
			Messages:        messages,
			Temperature:     &temp,
			MaxTokens:       maxTok, // (P7) our cap, resolved from Config. Too low silently clips a long answer/edit.
			ReasoningEffort: cfg.ReasoningEffort,
		})
		modelMs := time.Since(modelStart).Milliseconds()
		if err != nil {
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
		reply := strings.TrimSpace(resp.Text())
		cfg.Obs.Model(reply)
		res.Iterations = i
		res.Usage = addUsage(res.Usage, resp.Usage)
		cfg.Spend.Add(cfg.SolverModel, resp.Usage) // nil-safe; per-turn solver dollars
		noteUsage(cfg.Obs, i, res.Usage, cfg.MaxTotalTokens)
		ctxTok := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		notifyUsage(cfg.Obs, res.Usage, ctxTok)

		// The model's turn becomes part of the state we carry forward (P1).
		messages = append(messages, llm.Assistant(reply))

		// (P5) Did the model's hidden reasoning move this turn? Compare the opaque
		// trace to the previous turn's BEFORE updating the tracker. Empty trace
		// (non-thinking model) counts as not-advanced, so it keeps the strict threshold.
		// Deliberately NOT gated on ReasoningTokens > 0 — tried and REVERTED
		// 2026-06-12; see the twin comment in loop_tools.go (gemini moves its
		// encrypted thought-signature with zero reported tokens, and gating on the
		// token count false-killed real work: trace eval 5/5 → 0/5).
		// reasoningSignature is shared with the native loop (loop_tools.go) — one helper.
		reasoning := reasoningSignature(resp.Content)
		reasoningAdvanced := reasoning != "" && reasoning != lastReasoning
		lastReasoning = reasoning

		// The harness DISPOSES: we parse the proposed action ourselves (P2, P7).
		verb, arg := parseAction(reply, cfg.Tools)
		step := Step{Iter: i, Reply: reply, Verb: verb, Arg: arg, Usage: resp.Usage, ModelMs: modelMs, ReasoningAdvanced: reasoningAdvanced, FinishReason: resp.FinishReason}

		// ---- Principle 5: a done-signal the model can emit. ----
		if verb == "answer" {
			step.Grounded = grounded
			res.Steps = append(res.Steps, step)
			dec := gs.finish(ctx, finishInput{
				answer:               arg,
				lastRunFailed:        tr.lastRunFailed,
				canContinue:          i < maxIter,
				verifyContinuePhrase: "you answered",
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
				res.Outcome = dec.outcome
				res.Reason = dec.reason
				res.Answer = dec.answer
				return res, nil
			case finishAnswered:
				res.Outcome = Answered
				res.Answer = dec.answer
				res.memDone = dec.memDone
				return res, nil
			}
		}

		// ---- Principle 5: TWO no-progress detectors. ----
		// (a) tight loop: the same action (verb+arg) repeated. Two thresholds: a
		// thinking model whose reasoning ADVANCED this turn re-issues the same visible
		// action while its chain of thought moves (read -> think -> think -> act), so the
		// strict maxRepeats=2 false-kills it mid-thought; give it maxReasoningRepeats
		// before cutting. Frozen or absent reasoning is a real stall -> strict threshold.
		if verb+" "+arg == lastAction {
			repeats++
			if repeats >= repeatThreshold(reasoningAdvanced) {
				res.Steps = append(res.Steps, step)
				res.Outcome = KilledRepeat
				res.Reason = fmt.Sprintf("no progress: repeated %q %d times", lastAction, repeats)
				return gs.upgradeIfVerified(ctx, res), nil
			}
		} else {
			repeats = 0
		}
		lastAction = verb + " " + arg

		// (b) explore-spiral: discovery (list_dir/search) noProgressWindow turns
		// running, even with DIFFERENT args (list_dir a, b, c … or search x, y, z …)
		// — which (a) can't see. Keyed on the DISCOVERY CLASS: these tools return
		// pointers and commit to nothing, so grinding them means wandering, not
		// converging. read_file/edit/run is NOT discovery — a re-read/re-run of the
		// SAME arg is (a)'s job, DIFFERENT args (paging a file, stepping a pipeline)
		// are real progress, and a read after a search is reconnaissance that resets
		// the count. (Native loop mirrors this with allCallsDiscovery.)
		if discoveryTools[verb] {
			// Reasoning-aware window, mirroring the repeat detector and the native
			// loop — see spiralLimit.
			sameVerb++
			if sameVerb >= spiralLimit(spiralWindow, reasoningAdvanced) {
				res.Steps = append(res.Steps, step)
				res.Outcome = KilledSpiral
				res.Reason = fmt.Sprintf("no progress: %d discovery turns in a row (list_dir/search) — read or edit a specific target, or answer", sameVerb)
				return gs.upgradeIfVerified(ctx, res), nil
			}
		} else {
			sameVerb = 0
		}

		// ACT: run the named tool. The model only chose it; we execute it (P2).
		toolStart := time.Now()
		sig := verb + " " + arg
		readOnly := verb == "read_file" || verb == "search" || verb == "list_dir"
		var observation string
		if readOnly && sig == lastReadSig && sig == lastExecutedSig && !reasoningAdvanced {
			observation = "(skipped: identical to your previous read; result unchanged) " + lastReadObs
		} else {
			observation = dispatch(loopCtx, cfg.Tools, verb, arg)
			lastExecutedSig = sig
			if readOnly && !strings.HasPrefix(observation, "ERROR:") {
				lastReadSig = sig
				lastReadObs = observation
			}
		}
		rawObs := observation
		step.ToolMs = time.Since(toolStart).Milliseconds()

		// A successful tool observation means the model has now seen REAL external
		// state this run — anything it answers from here is grounded, so worth
		// remembering. Tool errors don't count (they aren't verified facts).
		if !strings.HasPrefix(observation, "ERROR:") {
			grounded = true
		}
		step.Grounded = grounded
		// step.Observation is recorded just before the observation is fed back
		// (after the diagnostics/churn/finish nudges below have augmented it), so
		// the persisted trace matches what the model actually read (review #5).

		// ---- Principle 5: stagnant-observation detector (shared tracker). ----
		if verb == "run" {
			if kill, count := tr.observeRun(observation); kill {
				res.Outcome = KilledStagnant
				res.Reason = fmt.Sprintf("no progress: the same command failure recurred %d times despite changing actions — the approach is stuck; change strategy or rewrite the file", count)
				step.Observation = observation // the kill returns before the shared record point below.
				res.Steps = append(res.Steps, step)
				cfg.Obs.Observation(observation)
				return gs.upgradeIfVerified(ctx, res), nil
			}
		}
		if tr.observeAction(i, verb) {
			// (code-intel slice 1) Diagnostics feed, only on edit turns — no point
			// re-checking a read/run turn. Appended to the observation the model
			// reads next (the text loop's wire format).
			if msg := tr.diagnostics(loopCtx, runTimeout); msg != "" {
				observation += "\n\n" + msg
			}
		}

		// The observation-repeat detector keys on RAW external state, not on the
		// hints we may append below. If count-varying nudges were included here, the
		// counter would reset before the count-6 KilledRepeat safety stop could fire.
		rawObservationFP := verb + " " + arg + "\nOBSERVATION:\n" + observation

		// (P3) Churn nudge, appended to whatever the current observation is so the
		// model reads it next turn.
		if tr.churnNudge() {
			observation += churnNudge
		}

		// Green-repeat nudge: the model keeps re-running the same passing command
		// with no file changes between. Appended to the observation the model
		// reads next — a nudge, never a kill.
		if tr.greenRepeatNudge() {
			observation += greenRepeatNudgeText
		}

		// (HP-4) Near-cap finisher (shared tracker), appended to THIS observation
		// so the model reads it next turn. See Config.FinishNudgeWindow.
		if tr.finishNudge(i) {
			observation += finishNudgeText
			cfg.Obs.Note("near cap with a green build and stable files — nudging to finish (HP-4)")
		}

		// Observation-repeat hard stop: if the SAME tool call keeps producing the
		// SAME raw observation, reasoning-token churn is not progress. Hints appended
		// above are deliberately excluded so escalation cannot mask the count-6 kill;
		// the action-only detector above still owns the earlier no-reasoning kill and
		// its message shape.
		repeatNudge := ""
		if kill, count := tr.observeToolObservation(rawObservationFP); kill {
			res.Outcome = KilledRepeat
			res.Reason = fmt.Sprintf("no progress: repeated %q %d times", verb+" "+arg, count)
			step.Observation = observation
			res.Steps = append(res.Steps, step)
			cfg.Obs.Observation(observation)
			return gs.upgradeIfVerified(ctx, res), nil
		} else {
			repeatNudge = escalatingRepeatNudge(count)
		}

		// (Lever 2b) Wire dedup-at-source — placed AFTER the repeat detector above so
		// the fingerprint still keys on raw bytes. Replaces only the billed copy; the
		// nudge/diagnostics suffix appended above rides along. See loop_tools.go.
		if s, dup := dd.stub(rawObs, i); dup {
			observation = s + strings.TrimPrefix(observation, rawObs)
		}

		// Record the step with the FINAL observation — after every augmentation —
		// so RunResult.Steps and the persisted transcript carry exactly the text
		// the model read, not a pre-nudge draft (review #5).
		step.Observation = observation
		res.Steps = append(res.Steps, step)

		// OBSERVE: the result — including any error — is appended as the next thing
		// the model sees (P2, P4). It is REAL external state, our anchor.
		cfg.Obs.Observation(observation)
		messages = append(messages, llm.User("OBSERVATION:\n"+observation))
		if repeatNudge != "" {
			messages = append(messages, llm.User(repeatNudge))
		}
	}

	// ---- Principle 5: if we fall out of the loop, WE stop it. Never trust the model to. ----
	res.Outcome = HitCap
	res.Reason = fmt.Sprintf("hit iteration cap (%d) without an answer", maxIter)
	return gs.upgradeIfVerified(ctx, res), nil
}

// churnNudge is the one-time hint appended to an observation once a session crosses
// Config.ChurnNudgeRuns failing test-runs OR edit_file calls (P3). The live runs
// showed capable cheap models pass when they write the file wholesale but burn the
// whole iteration budget when they wander — gpt-oss in repeated failing test-runs,
// grok in read/edit churn (barely running the tests) — so a stuck model is steered
// toward the path that works, regardless of which way it is spinning.
const churnNudge = "\n\n[hint: the tests have failed several times. If you've been making incremental edits, " +
	"stop and rewrite the whole file in ONE write_file with a complete, correct implementation — " +
	"that is usually faster than chasing line-by-line edits, and avoids line-number drift.]"

// greenRepeatNudgeText is the one-time hint appended to an observation when the
// model re-runs the SAME passing command 3+ times with no file changes between
// (see turnTracker.greenRepeatNudge). A thinking model that keeps running a green
// test without touching files is spinning — it read the result, thought, and ran
// again rather than acting on the green signal. This nudges it to finish or take a
// genuinely new action, without killing (the false-kill lesson from the
// two-threshold detector).
const greenRepeatNudgeText = "\n\n[harness: the same command has now passed 3 times with no file changes in between — " +
	"re-running it gains nothing; either finish (answer) or take a genuinely new action.]"

func escalatingRepeatNudge(count int) string {
	switch count {
	case 3:
		return "[harness: you have produced the identical tool result 3 times. If you are just confirming a green state, stop and either finish or take a genuinely different action.]"
	case 4:
		return "[harness: this is now 4 identical tool-result turns; repeating it again will not change the outcome. Finish if the work is done, or take a genuinely different action.]"
	case 5:
		return "[harness: this is the 5th identical tool-result turn; the next identical turn ENDS the run. Write your FINAL answer / bank your patch NOW.]"
	default:
		return ""
	}
}

// finishNudgeText / finishNudgeNative are HP-4's one-time near-cap finisher hint
// (see Config.FinishNudgeWindow): injected once a session is within the window of the
// cap with a green last `run` and stable files. They differ ONLY in how each loop
// finishes — the text loop answers with the `answer` verb, the native loop stops
// calling tools and replies in plain text — so each names the right finish move for
// its protocol. finishNudgeText is appended to a text observation (hence the leading
// blank line); finishNudgeNative is a standalone user message (no leading newlines).
const finishNudgeText = "\n\n[hint: your last build/test run passed and you haven't edited any files for several turns — " +
	"the task may already be complete. If it is, FINISH NOW with `answer <one-line summary of what you did>`. " +
	"If something still remains, say what and keep working.]"

const finishNudgeNative = "[hint: your last build/test run passed and you haven't edited any files for several turns — " +
	"the task may already be complete. If it is, FINISH NOW: reply with your final answer as plain text and do NOT call a tool. " +
	"If something still remains, keep working.]"

// answerNudgeNative is the near-cap answer-forcer hint (see Config.AnswerNudgeWindow)
// for an observe-only agent that has no build signal to gate the finisher on. It
// tells the model to stop reading and answer from what it has, because an unanswered
// run wastes the whole budget.
// finishToolNudgeWindow is how close to the iteration cap the finish-tool
// reminder fires (DUET-DOGFOOD F2): 2 leaves the model one turn to absorb the
// hint and one to act on it. A fixed window, not a Config knob — the reminder is
// safe whenever a FinishTool is configured (it can't mask broken state), so
// there's nothing for a caller to tune.
const finishToolNudgeWindow = 2

const answerNudgeNative = "[hint: you are almost out of turns. STOP exploring now and give your FINAL answer as plain text — " +
	"do NOT call another tool. Answer from what you have already read; an unanswered run produces nothing.]"

// dispatch runs a tool and turns ANY failure into an observation string (P6).
// A tool error is information for the model, not a crash for us.
func dispatch(ctx context.Context, tools map[string]Tool, verb, arg string) string {
	t, ok := tools[verb]
	if !ok {
		// (P6) feedback, not a crash — but it must list `answer`. Dogfooding showed
		// the model reach the right conclusion ("config.yaml does not exist") in
		// prose, get this error WITHOUT `answer` in it, and so steer back to tools
		// instead of finishing. Naming `answer` and showing how to wrap a conclusion
		// closes that gap (the absence-affordance from the prompt needs a verb to land).
		return fmt.Sprintf("ERROR: no action recognized — your line must START with `answer` or a tool (%s). "+
			"To finish, prefix your conclusion with `answer` (e.g. `answer config.yaml does not exist`); a confirmed absence is a valid answer.",
			strings.Join(toolNames(tools), ", "))
	}
	out, err := t.Run(ctx, arg)
	if err != nil {
		return "ERROR: " + err.Error() // (P6) the model will see this and can correct next turn.
	}
	return truncate(out) // (P3) keep observations bounded so the context stays healthy.
}

// parseAction reads the model's text and extracts the FIRST recognized command.
// We tolerate the model being chatty by scanning for a known verb prefix —
// the harness adapts to the model, not the other way around (P7).
func parseAction(reply string, tools map[string]Tool) (verb, arg string) {
	// Derive known verbs from the ACTUAL toolset (+ the built-in "answer") so
	// adding a tool never desyncs the parser from the prompt (cf. sorted toolNames).
	known := map[string]bool{"answer": true}
	for name := range tools {
		known[name] = true
	}
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		v, rest, _ := strings.Cut(line, " ")
		if known[v] {
			return v, strings.TrimSpace(rest)
		}
	}
	// No recognized action: treat the whole reply as a malformed attempt so the
	// no-progress / cap logic still governs it.
	return "", reply
}

// buildSystemPrompt teaches the model the protocol. The structure is dictated
// by us (P7); the model only fills in the next single action.
func buildSystemPrompt(tools map[string]Tool) string {
	var b strings.Builder
	b.WriteString("You are a tool-using agent. Each turn, respond with EXACTLY ONE line, no extra text.\n")
	b.WriteString("Choose one action:\n")
	// Iterate in sorted order: ranging a map directly gives a RANDOM order each
	// run (Go does this on purpose), which would make the prompt — and thus the
	// "temp=0 deterministic" run — non-reproducible. Never rely on map order.
	for _, name := range toolNames(tools) {
		t := tools[name]
		fmt.Fprintf(&b, "  %s <argument>   — %s\n", t.Name, t.Desc)
	}
	b.WriteString("  answer <final answer>   — when you have enough information to finish\n\n")
	b.WriteString("You will receive an OBSERVATION after each action. Base your next action on it. " +
		"Pick the action that most directly advances the TASK — if the task names an operation (test, search, build), use run for it rather than exploring. " +
		"Confirm a path with list_dir only when you're about to read one you're unsure of. " +
		"If you've checked and something genuinely isn't there, saying so is a valid answer.")
	return b.String()
}
