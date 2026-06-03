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
	if refusal := checkIsolation(cfg); refusal != nil {
		return refusal, nil // (P2/§5) too-weak sandbox — refuse before the first model call.
	}
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}
	// (P1 spine) Same run-identity stamping as Run: ID + wall-clock on every exit path.
	runID, startedAt := newRunID(), time.Now()
	defer func() { stampRun(out, runID, startedAt) }()
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

	// (P1) State lives HERE; we re-send the whole conversation each turn.
	messages := []llm.Message{llm.User("TASK: " + cfg.Task)}
	// (P3) Recalled long-term memory rides in the system prompt, labelled stale.
	system := nativeSystemPrompt() + recall(ctx, cfg.Memory, cfg.Task)
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

	// (P5) Stagnant-observation state, evaluated on each `run` result regardless of
	// which turn it lands in: lastRunFP fingerprints the most recent failing `run`
	// (duration stripped), stagnant counts identical recurrences, lastRunFailed feeds
	// the verification fallback.
	var lastRunFP string
	stagnant := 0
	lastRunFailed := false
	failRuns := 0   // count of failing `run` results this session (a churn signal).
	edits := 0      // count of edit_file calls this session (the other churn signal).
	nudged := false // the churn nudge fires at most once.

	// (code-intel slice 1) edits since the last green build/run — the diagnostics-feed
	// stuck signal (reset on any passing `run` or a clean DiagnoseCmd). See DiagnoseCmd.
	editsSinceGreen := 0

	// (HP-4) Near-cap finisher state, mirroring the text loop: lastRunPassed is the
	// "build/test green" half of the done-signal, lastEditIter the iteration of the
	// most recent file mutation (0 = none), finishNudged the once-only latch.
	lastRunPassed := false
	lastEditIter := 0
	finishNudged := false
	answerNudged := false

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
			return upgradeIfVerified(cfg, res, runTimeout), nil
		}
		cfg.Obs.Iteration(i, maxIter)

		// generateWithEviction adds HP-1's reactive fallback: on a window overflow it
		// compacts the OLDEST turn (pairing-safe) and retries instead of crashing,
		// returning the possibly-shrunk transcript we carry forward.
		var resp *llm.Response
		var err error
		resp, messages, err = generateWithEviction(loopCtx, cfg, llm.Request{
			System:      system,
			Messages:    messages,
			Tools:       schemas,
			Temperature: &temp,
			MaxTokens:   maxTok,
		})
		if err != nil {
			// A deadline hit mid-Generate is the wall-clock budget, not a transport fault.
			if cfg.MaxWallClock > 0 && loopCtx.Err() == context.DeadlineExceeded {
				res.Outcome = HitDeadline
				res.Reason = fmt.Sprintf("hit wall-clock budget (%s) mid-turn", cfg.MaxWallClock)
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
			// (HP-1) Window overflowed and eviction couldn't compact it further —
			// degrade gracefully rather than mislabel it a transport fault.
			if errors.Is(err, llm.ErrContextLength) {
				res.Outcome = HitContextLimit
				res.Reason = "context window exceeded and could not be compacted further"
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
			res.Outcome, res.Reason, res.Err = ProviderErr, err.Error(), err
			return res, err
		}
		res.Iterations = i
		res.Usage = addUsage(res.Usage, resp.Usage)

		calls := toolCalls(resp.Content)

		// (P5) Termination: the model stopped calling tools and answered in prose.
		if len(calls) == 0 {
			answer := strings.TrimSpace(resp.Text())
			res.Steps = append(res.Steps, Step{Iter: i, Reply: answer, Verb: "answer", Arg: answer, Grounded: grounded, Usage: resp.Usage})
			// (P5/HP-5) A tool-call-free turn is the done-signal — but it fires even
			// when the model narrated intent, acknowledged failure, or hallucinated
			// success (DOGFOOD R9/R10, the most common false-positive in the bake-offs).
			// Re-verify the claimed state before accepting it.
			reason := verifyTermination(ctx, cfg, lastRunFailed, runTimeout)
			if reason != "" && cfg.VerifyContinue && i < maxIter {
				// Continue-on-fail: re-ground with the real failing state (P4) and keep
				// working rather than accept a premature finish. The assistant's
				// tool-call-free turn must precede the feedback so the conversation stays
				// well-formed.
				cfg.Obs.Note("finish rejected (not verified) — continuing")
				messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})
				messages = append(messages, llm.User("OBSERVATION:\nNot finished — you stopped calling tools, but the task is not verified:\n"+reason+"\nKeep working: fix the code and re-run until it passes."))
				continue
			}
			if reason != "" {
				res.Outcome, res.Answer, res.Reason = Unverified, answer, reason
				cfg.Obs.Note("answer not verified — " + reason)
				return res, nil
			}
			res.Outcome, res.Answer = Answered, answer
			cfg.Obs.Done(answer)
			if grounded {
				remember(ctx, cfg.Memory, cfg.Task, answer)
			} else if cfg.Memory != nil {
				cfg.Obs.Note("memory: answer not tool-verified this run — not stored (avoids amplifying guessed/recalled facts)")
			}
			return res, nil
		}

		cfg.Obs.Model(callsSummary(calls, cfg.Tools))
		// (P1) The assistant turn — carrying its tool-call parts — becomes state we
		// re-send; the API requires the tool_calls to precede their results.
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Content})

		// (P5) Two no-progress detectors, the same intent as the text loop but
		// evaluated PER TURN (a native turn may legitimately fan out several calls):
		// (a) tight loop — the model re-issues the IDENTICAL turn over and over;
		// (b) explore-spiral — consecutive turns that do nothing but navigate with
		//     list_dir, never escalating to run/read_file or answering. A turn that
		//     mixes list_dir with any other tool is progress and resets the count, and
		//     a single turn that fans out N parallel list_dir calls is ONE nav turn,
		//     not N — so real parallel exploration is never mistaken for a spiral.
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
		reasoningSig := reasoningSignature(resp.Content)
		if sig == lastTurnSig { // (a)
			// Pick the threshold by whether the model is actively reasoning: a turn
			// whose reasoning trace MOVED gets the lenient ceiling (hidden progress —
			// don't false-kill it mid-thought), a frozen or absent trace gets the strict
			// one (a true tight loop). Either way the spiral is bounded — the lenient
			// ceiling still cuts a 20x re-read long before the iteration cap.
			limit := maxRepeats
			if reasoningSig != lastReasoningSig {
				limit = maxReasoningRepeats
			}
			if repeats++; repeats >= limit {
				res.Steps = append(res.Steps, turnSteps(i, calls, cfg.Tools, grounded, resp.Usage)...)
				res.Outcome = KilledRepeat
				res.Reason = fmt.Sprintf("no progress: repeated %q %d times", sig, repeats)
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
		} else {
			repeats = 0
		}
		lastTurnSig = sig
		lastReasoningSig = reasoningSig

		if allCallsListDir(calls) { // (b)
			if navRun++; navRun >= spiralWindow {
				res.Steps = append(res.Steps, turnSteps(i, calls, cfg.Tools, grounded, resp.Usage)...)
				res.Outcome = KilledSpiral
				res.Reason = fmt.Sprintf("no progress: %d list_dir-only turns in a row — switch to run/read_file, or answer", navRun)
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
		} else {
			navRun = 0
		}

		// ACT: run every requested call in order, appending one result per id (a
		// missing result for any call would make the next request malformed).
		usageForTurn := resp.Usage // attribute the turn's usage to its first step only.
		editedThisTurn := false    // (code-intel slice 1) did any call this turn mutate a file?
		for _, c := range calls {
			step := Step{Iter: i, Verb: c.Name, Arg: callArg(c, cfg.Tools), Usage: usageForTurn}
			usageForTurn = llm.Usage{}

			obs, isErr := dispatchNative(loopCtx, cfg.Tools, c)
			if !isErr {
				grounded = true // (P4) the model has now seen real external state.
			}

			// (P5) Stagnant-observation + churn tracking, evaluated on `run` results.
			// Done BEFORE feeding the observation back so the churn nudge can ride the
			// very message the model reads. A `run` that keeps failing with the
			// byte-identical result, across turns whose ACTIONS differ, is a stall the
			// turn-signature/spiral detectors can't see (DOGFOOD R9/R10's 30-turn
			// edit→test→edit→same-error burn) — key it on the observation, not the action.
			kill := false
			if c.Name == "run" {
				lastRunFailed = isRunFailure(obs)
				lastRunPassed = isRunSuccess(obs) // (HP-4) the green/red of the most recent run.
				if lastRunPassed {
					editsSinceGreen = 0 // (code-intel slice 1) reaching green clears the diagnostics-stuck count.
				}
				if lastRunFailed {
					failRuns++
				}
				switch {
				case !lastRunFailed: // a passing run is real progress — reset.
					stagnant, lastRunFP = 0, ""
				case runFingerprint(obs) == lastRunFP:
					stagnant++
				default: // a NEW failure — the world changed, count restarts.
					stagnant, lastRunFP = 1, runFingerprint(obs)
				}
				kill = stagnant >= maxStagnant
			}
			if c.Name == "edit_file" {
				edits++
			}
			if c.Name == "write_file" || c.Name == "edit_file" {
				lastEditIter = i // (HP-4) a file mutation resets the "files stable" clock.
				editsSinceGreen++
				editedThisTurn = true
			}
			// (P3) Churn nudge: fire once when EITHER wandering signal crosses the
			// threshold — repeated failing test-runs (gpt-oss) OR many edit_file calls
			// without converging (grok, which barely runs the tests so a run-only
			// trigger never fires). Skipped on a kill turn (we're terminating anyway).
			if !kill && !nudged && cfg.ChurnNudgeRuns > 0 && (failRuns >= cfg.ChurnNudgeRuns || edits >= cfg.ChurnNudgeRuns) {
				obs += churnNudge
				nudged = true
			}

			step.Grounded = grounded
			step.Observation = obs
			res.Steps = append(res.Steps, step)
			cfg.Obs.Observation(obs)
			// (P6) A tool failure is an observation the model reacts to, tagged so
			// the provider marks it an error result — never a crash for us.
			messages = append(messages, llm.ToolResultMsg(c.ID, obs, isErr))

			if kill {
				res.Outcome = KilledStagnant
				res.Reason = fmt.Sprintf("no progress: the same command failure recurred %d times despite changing actions — the approach is stuck; change strategy or rewrite the file", stagnant)
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
		}

		// (code-intel slice 1) Diagnostics feed, once per turn after the tool results are in
		// the transcript (so the conversation stays well-formed, like the finisher below): if
		// the model edited this turn and is stuck — DiagnoseAfterEdits edits without a green
		// build — run the diagnostics SOURCE and surface its errors as a standalone user
		// message, INFORMATION not a gate (see CODE-INTELLIGENCE.md). A clean build means it
		// reached green via the check, so reset and stay silent.
		if cfg.DiagnoseCmd != "" && cfg.DiagnoseAfterEdits > 0 && editedThisTurn &&
			editsSinceGreen >= cfg.DiagnoseAfterEdits {
			if report, clean := diagnoseSource(loopCtx, cfg, runTimeout); clean {
				editsSinceGreen = 0
			} else {
				messages = append(messages, llm.User(diagnosticsMessage(cfg.DiagnoseCmd, report)))
				cfg.Obs.Note("stuck with a broken build — surfacing diagnostics (code-intel slice 1)")
			}
		}

		// (HP-4) Near-cap finisher, evaluated once per TURN (after all of this turn's
		// tool results are in the transcript, so the conversation stays well-formed):
		// when the budget is nearly spent AND the world looks settled — the last `run`
		// was green and the files have been stable for the window — append a standalone
		// hint manufacturing the finish ATTEMPT the spinner never makes. The model reads
		// it next turn; the i < maxIter guard leaves it a turn to act. See
		// Config.FinishNudgeWindow.
		if !finishNudged && cfg.FinishNudgeWindow > 0 && i < maxIter &&
			maxIter-i <= cfg.FinishNudgeWindow &&
			lastRunPassed && i-lastEditIter >= cfg.FinishNudgeWindow {
			cfg.Obs.Note("near cap with a green build and stable files — nudging to finish (HP-4)")
			messages = append(messages, llm.User(finishNudgeNative))
			finishNudged = true
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
	}

	res.Outcome = HitCap
	res.Reason = fmt.Sprintf("hit iteration cap (%d) without an answer", maxIter)
	return upgradeIfVerified(cfg, res, runTimeout), nil
}

// observeOnlyTools is the allowlist of built-in tools that cannot mutate state or
// execute code. isObserveOnly checks membership against it rather than denylisting
// known effect tools, so it FAILS CLOSED: a custom or future effectful tool the
// harness doesn't know about is NOT observe-only (dogfood slice 4, round 3, O3).
var observeOnlyTools = map[string]bool{"list_dir": true, "read_file": true, "search": true}

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

// allCallsListDir reports whether EVERY call in a turn is list_dir — a pure
// navigation turn. Gated to list_dir for the same reason as the text loop: it is
// the only pure-navigation tool, so a run of list_dir-only turns means wandering,
// while repeated read_file/run is either the tight-loop detector's job or real
// paging/progress. A turn mixing list_dir with another tool is NOT pure nav.
func allCallsListDir(calls []llm.ToolCallPart) bool {
	for _, c := range calls {
		if c.Name != "list_dir" {
			return false
		}
	}
	return len(calls) > 0
}

// turnSteps builds the trace steps for a turn killed by a detector BEFORE
// dispatch (so they carry no observation): one Step per call, the turn's usage
// attributed to the first, all flagged with the run's current grounded state.
func turnSteps(iter int, calls []llm.ToolCallPart, tools map[string]Tool, grounded bool, usage llm.Usage) []Step {
	steps := make([]Step, len(calls))
	for i, c := range calls {
		u := llm.Usage{}
		if i == 0 {
			u = usage
		}
		steps[i] = Step{Iter: iter, Verb: c.Name, Arg: callArg(c, tools), Grounded: grounded, Usage: u}
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
