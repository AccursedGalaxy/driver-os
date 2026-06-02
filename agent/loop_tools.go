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
	"fmt"
	"strings"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// RunNative executes the agent against a tool-capable provider. Its signature and
// RunResult match Run exactly, so a caller swaps loops without other changes.
func RunNative(ctx context.Context, cfg Config) (*RunResult, error) {
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}
	maxTok := cfg.MaxTokens
	if maxTok <= 0 {
		maxTok = DefaultMaxTokens
	}
	runTimeout := cfg.RunTimeout
	if runTimeout <= 0 {
		runTimeout = defaultRunTimeout
	}
	if cfg.Tools == nil {
		cfg.Tools = DefaultTools(cfg.Sandbox, runTimeout)
	}

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
	repeats, navRun := 0, 0
	grounded := false // (P4) gates memory writes — only a verified answer is stored.

	for i := 1; i <= maxIter; i++ {
		cfg.Obs.Iteration(i, maxIter)

		resp, err := cfg.Model.Generate(ctx, llm.Request{
			System:      system,
			Messages:    messages,
			Tools:       schemas,
			Temperature: &temp,
			MaxTokens:   maxTok,
		})
		if err != nil {
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
		if sig == lastTurnSig { // (a)
			if repeats++; repeats >= maxRepeats {
				res.Steps = append(res.Steps, turnSteps(i, calls, cfg.Tools, grounded, resp.Usage)...)
				res.Outcome = KilledRepeat
				res.Reason = fmt.Sprintf("no progress: repeated %q %d times", sig, repeats)
				return res, nil
			}
		} else {
			repeats = 0
		}
		lastTurnSig = sig

		if allCallsListDir(calls) { // (b)
			if navRun++; navRun >= noProgressWindow {
				res.Steps = append(res.Steps, turnSteps(i, calls, cfg.Tools, grounded, resp.Usage)...)
				res.Outcome = KilledSpiral
				res.Reason = fmt.Sprintf("no progress: %d list_dir-only turns in a row — switch to run/read_file, or answer", navRun)
				return res, nil
			}
		} else {
			navRun = 0
		}

		// ACT: run every requested call in order, appending one result per id (a
		// missing result for any call would make the next request malformed).
		usageForTurn := resp.Usage // attribute the turn's usage to its first step only.
		for _, c := range calls {
			step := Step{Iter: i, Verb: c.Name, Arg: callArg(c, cfg.Tools), Usage: usageForTurn}
			usageForTurn = llm.Usage{}

			obs, isErr := dispatchNative(ctx, cfg.Tools, c)
			if !isErr {
				grounded = true // (P4) the model has now seen real external state.
			}
			step.Grounded = grounded
			step.Observation = obs
			res.Steps = append(res.Steps, step)
			cfg.Obs.Observation(obs)
			// (P6) A tool failure is an observation the model reacts to, tagged so
			// the provider marks it an error result — never a crash for us.
			messages = append(messages, llm.ToolResultMsg(c.ID, obs, isErr))
		}
	}

	res.Outcome = HitCap
	res.Reason = fmt.Sprintf("hit iteration cap (%d) without an answer", maxIter)
	return res, nil
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
