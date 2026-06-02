// Package agent is the think -> act -> observe loop, extracted from cmd/agent so
// it can be both DRIVEN by a CLI and MEASURED by an eval harness (HP-11). The
// loop prints nothing itself: progress flows through an Observer, and the
// terminal state flows back as a structured RunResult — so one run produces data
// (a trace + a typed outcome), not stdout noise. cmd/agent wires a printing
// Observer to reproduce the old live output exactly; the eval harness passes a
// silent one and reads the RunResult.
//
// It implements the cycle in the most transparent way possible: no native
// function-calling, no framework. The model emits ONE line of plain text; OUR
// code parses it, runs the tool, and feeds the result back. Every dial below is
// annotated with which of the seven principles it embodies.
//
// The seven principles, in one breath:
//  1. State lives in YOUR code, never in the model. The LLM is (context) -> text.
//  2. The cycle is think -> act -> observe (the model proposes, the harness disposes).
//  3. Context is the only state, so managing it IS the engineering.
//  4. Every iteration must observe REAL external state, not its own prior text.
//  5. Termination is YOUR job, and needs multiple conditions.
//  6. Tool failures are observations, not crashes.
//  7. The control flow is yours; the model only fills in the next action.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AccursedGalaxy/mneme"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// ---- Principle 7: the control flow is yours. These are OUR dials, not the model's. ----
const (
	// DefaultMaxIterations is the (P5) hard cap when Config.MaxIterations is unset:
	// non-negotiable termination, prevents infinite spend. Exported so the CLI can
	// use it as a flag default. A longer/complex task raises this via Config.
	DefaultMaxIterations = 8
	// DefaultMaxTokens caps a single model turn's output when Config.MaxTokens is
	// unset. Too low silently clips a long final answer — or a write_file/edit_file
	// content block — mid-sentence (P7: our knob, not the model's).
	DefaultMaxTokens = 1024

	maxRepeats = 2 // (P5) tight-loop detector: the SAME action this many times -> kill.

	// maxStagnant is the (P5) stagnant-OBSERVATION detector: the SAME failing `run`
	// result this many times KILLS the run, even when the actions producing it
	// differ. The repeat/spiral detectors above key on the model's ACTION (same
	// verb+arg, or list_dir-only turns); both missed the DOGFOOD R9/R10 pathology
	// where a weak model made N distinct edit_file/run turns that each left the build
	// broken with the byte-identical error — productive-looking churn, zero progress.
	// Keying on an unchanging failing observation (not the action) catches it: the
	// world isn't moving even though the model is. 3 lets a genuine retry-after-fix
	// through (a real fix changes the error) while ending a true stall early.
	maxStagnant = 3

	// noProgressWindow is the wider net (P5). The exact-repeat detector misses the
	// spiral dogfooding exposed: the model grinds list_dir with DIFFERENT args
	// (list_dir a, list_dir b, list_dir c …), never escalating to run/read_file or
	// answering — same verb, different arg, so exact-match never fires and it burns
	// the whole budget. This kills a run of list_dir calls regardless of arg (see
	// the detector for why it's gated to list_dir, not any repeated verb).
	noProgressWindow = 4

	// observationCap is the FINAL BACKSTOP, not the policy. Each tool now shapes
	// its own output to the minimum the model needs (P1, "return information not
	// data"); this generous rune cap only catches a pathological case that slips
	// past per-tool bounds. HP-1 rightly calls a blind global cap "the dumbest
	// possible policy" — so it stops being the policy. It also clips head+tail,
	// not head-only, so a tool's trailing recovery note survives a backstop trim.
	observationCap = 12000

	// Two DIFFERENT bounds, conflated in casual "bound everything" talk (P4 vs P1):
	maxFileBytes = 1 << 20 // (P4, MEMORY) read_file never pulls more than 1 MiB off disk — the OOM fence.
	readLineCap  = 150     // (P1, CONTEXT) read_file returns at most this many lines unless a range asks for fewer.
	listEntryCap = 200     // (P1) list_dir caps entries so a huge directory can't flood the window.
	runStreamCap = 4000    // (P1) run clips each of stdout/stderr to this many runes, head+tail.

	// defaultRunTimeout bounds a single `run` command when Config.RunTimeout is
	// unset (P5: a runaway command is the sandbox's job to kill). A real build or
	// test suite can exceed it — raise it via Config for longer-running work.
	defaultRunTimeout = 30 * time.Second

	// writeByteCap is the BACKSTOP on a single write_file, not its policy (cf.
	// observationCap). A turn's content can't realistically exceed the model's own
	// MaxTokens output cap, so this never fires on the live loop — it exists to stop
	// a non-model caller (Tool is exported for custom toolsets) or a future-larger
	// output cap from dumping an unbounded blob to disk in one action (P4, the disk
	// analogue of the maxFileBytes read fence).
	writeByteCap = 64 << 10 // 64 KiB per write_file call.
)

// Tool is a thing the harness can do on the model's behalf. The model NEVER
// touches the real world directly (P2) — it only names a tool and an argument;
// our code runs it. Exported so an eval (or any caller) can supply a custom
// toolset; Config.Tools nil falls back to DefaultTools.
type Tool struct {
	Name string
	Desc string                                                // TEXT loop description: states the tool AND its one-line ARG grammar + \n escapes — that framing IS the text protocol.
	Run  func(ctx context.Context, arg string) (string, error) // TEXT loop (and the native bridge fallback): the model fills one string.

	// NativeDesc is the tool-level description the STRUCTURED native loop
	// advertises (via nativeSchemas). It is behavior-only — what the tool does and
	// when to pick it — and deliberately omits the ARG grammar and the \n/\t/\\
	// escapes that Desc carries: in native mode those are FALSE (args are typed
	// JSON fields with real newlines, no escaping) and the per-field Schema
	// descriptions own the format. Leaking the text-protocol framing here is a
	// trap — a model that obeys "write a line break as \n" would write a literal
	// backslash-n into the verbatim structured content. Empty => nativeSchemas
	// falls back to Desc (fine for a custom tool whose Desc has no escape framing).
	NativeDesc string

	// Schema and RunJSON are the STRUCTURED native path. When both are set,
	// RunNative advertises Schema (typed, multi-field args) and dispatches the
	// model's JSON args straight to RunJSON — no single-string parsing in native
	// mode. They are optional and additive: a Tool with only Run still works in
	// both loops (the native loop bridges it to a one-string `arg` schema), so
	// custom/external toolsets are unaffected.
	Schema  json.RawMessage
	RunJSON func(ctx context.Context, args json.RawMessage) (string, error)
}

// Outcome is the typed terminal state of a Run. It replaces the old
// error-string encoding so a caller (the eval oracle) can branch on HOW a run
// ended — answered vs hit-cap vs which detector killed it — without parsing
// prose. KilledSpiral vs KilledRepeat is exactly the signal that told us fix-3
// needed gating last dogfood round, so it is first-class, not inferred.
type Outcome string

const (
	Answered       Outcome = "answered"        // the model emitted `answer` (and it verified, if a check was configured).
	Unverified     Outcome = "unverified"      // the model finished, but the closing verification (VerifyCmd / last-run check) failed — a non-pass (P5/HP-5).
	HitCap         Outcome = "hit_cap"         // ran out of iterations (P5 hard cap).
	KilledRepeat   Outcome = "killed_repeat"   // exact same action maxRepeats times.
	KilledSpiral   Outcome = "killed_spiral"   // noProgressWindow list_dir calls in a row.
	KilledStagnant Outcome = "killed_stagnant" // the same failing `run` result maxStagnant times despite changing actions.
	ProviderErr    Outcome = "provider_error"  // a transport/auth failure talking to the model.
)

// Step is one think->act->observe iteration, captured as data. The trace of
// Steps is what lets an eval assert BEHAVIOR (did it escalate to `run`? did it
// avoid the spiral?), not just final-answer correctness — because HP-2/HP-3/HP-7
// are behavior problems.
type Step struct {
	Iter        int       // 1-based iteration number.
	Reply       string    // the model's full reply this turn.
	Verb        string    // parsed action verb ("" if unrecognized).
	Arg         string    // parsed action argument.
	Observation string    // the tool result fed back (empty on the answer turn).
	Grounded    bool      // had any tool returned a real observation by end of this step.
	Usage       llm.Usage // token accounting for THIS turn's model call.
}

// RunResult is the structured outcome of a Run: the typed Outcome, the answer
// (if any), the full Step trace, summed token Usage, and the Root the sandbox
// ran against. Root is the fixture hook (HP-11): a baseline diff must know
// whether a case ran against an immutable fixture or the live repo, so the
// working dir travels with the result instead of being implicit.
type RunResult struct {
	Task       string
	Root       string  // the dir the sandbox was rooted at (Config.Root).
	Outcome    Outcome // how the run ended.
	Answer     string  // the final answer, set iff Outcome == Answered.
	Reason     string  // human explanation for a non-Answered outcome (kept so the CLI prints the old message verbatim).
	Steps      []Step  // the full trace.
	Iterations int     // turns taken.
	Usage      llm.Usage
	Err        error // set iff Outcome == ProviderErr.
}

// Config is everything Run needs. Model, Sandbox, and Task are required; the
// rest default sensibly (nil Tools -> DefaultTools, nil Memory -> no cross-run
// recall, nil Obs -> silent).
type Config struct {
	Model   llm.Provider    // required: the (context) -> text engine.
	Sandbox sandbox.Sandbox // required: the isolation boundary every effect flows through (P2).
	Memory  mneme.Memory    // optional: cross-run long-term memory; nil = stateless.
	Tools   map[string]Tool // optional: nil = DefaultTools(Sandbox).
	Task    string          // required: the goal.
	Root    string          // optional: the dir Sandbox is rooted at; recorded in RunResult.Root.
	Obs     Observer        // optional: live progress sink; nil = silent.

	// All three default sensibly when zero (P5/P7 — termination knobs are OURS):
	MaxIterations int           // 0 = DefaultMaxIterations. The hard cap on think->act->observe turns.
	MaxTokens     int           // 0 = DefaultMaxTokens. Per-turn output cap on the model call.
	RunTimeout    time.Duration // 0 = defaultRunTimeout. Wall-clock kill for a single `run` command.

	// VerifyCmd is the closing VERIFICATION gate (P5/HP-5): a success command the
	// caller names (e.g. "go test ./...") that is re-run when the model finishes.
	// A non-zero exit downgrades the terminal Answered to Unverified — turning a
	// model that stopped while the task was still broken (DOGFOOD R9/R10's
	// termination-by-silence false positives: narrated intent, acknowledged
	// failure, hallucinated success) into an honest non-pass instead of exit-0
	// success. Empty = no closing check. The harness does NOT guess the success
	// criterion; the caller states it.
	VerifyCmd string

	// VerifyLastRun is the no-VerifyCmd FALLBACK: when set (and VerifyCmd is empty),
	// a silent finish is marked Unverified if the most recent `run` this session was
	// still failing and nothing succeeded after it. Off by default and opt-in
	// because a legitimate absence answer often follows a non-zero exit (e.g. `grep`
	// returns 1 on no match), which this would wrongly flag — VerifyCmd is the
	// precise gate, this is the cheap heuristic for an un-instrumented run.
	VerifyLastRun bool
}

// Run is the entire agent. Notice it is tiny — the loop is trivial (P3); the
// interesting work is the context policy and the termination conditions. It
// prints nothing: events go to cfg.Obs, the terminal state to the returned
// RunResult. err is non-nil ONLY for a genuine infrastructure failure (the model
// call itself failed); a no-progress kill or a hit cap is a normal Outcome, not
// a Go error.
func Run(ctx context.Context, cfg Config) (*RunResult, error) {
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}

	// Resolve the OUR-side knobs from cfg-or-default (P5/P7). Done once, up front,
	// so the loop body reads from locals and the defaults live in exactly one place.
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

	// ---- Principle 1: STATE LIVES HERE, in our slice. The model holds nothing. ----
	// We rebuild and re-send this whole conversation on every single call.
	messages := []llm.Message{llm.User("TASK: " + cfg.Task)}

	// ---- Principle 3: context IS the state. Long-term memory from PAST runs
	// (mneme) is surfaced into the system prompt before we think. The model gets
	// what it learned before, but labelled as possibly-stale so it still verifies. ----
	system := buildSystemPrompt(cfg.Tools) + recall(ctx, cfg.Memory, cfg.Task)
	temp := 0.0 // deterministic-ish; this is our knob, not the model's (P7).

	var lastAction, lastVerb string
	repeats := 0
	sameVerb := 0

	// (P5) Stagnant-observation state + the last-run-failed flag the verification
	// fallback reads. lastRunFP is the fingerprint of the most recent failing `run`
	// result (duration stripped — see runFingerprint); stagnant counts how many
	// times in a row that identical failure has recurred.
	var lastRunFP string
	stagnant := 0
	lastRunFailed := false

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

	for i := 1; i <= maxIter; i++ { // (P5) the hard cap lives in the loop header.
		cfg.Obs.Iteration(i, maxIter)

		// THINK: send the FULL context (P1) and get back text. Pure function.
		resp, err := cfg.Model.Generate(ctx, llm.Request{
			System:      system,
			Messages:    messages,
			Temperature: &temp,
			MaxTokens:   maxTok, // (P7) our cap, resolved from Config. Too low silently clips a long answer/edit.
		})
		if err != nil {
			// A transport/auth failure is a real stop (tool errors are not — see
			// dispatch). Record it as a typed outcome AND return the error.
			res.Outcome = ProviderErr
			res.Reason = err.Error()
			res.Err = err
			return res, err
		}
		reply := strings.TrimSpace(resp.Text())
		cfg.Obs.Model(reply)
		res.Iterations = i
		res.Usage = addUsage(res.Usage, resp.Usage)

		// The model's turn becomes part of the state we carry forward (P1).
		messages = append(messages, llm.Assistant(reply))

		// The harness DISPOSES: we parse the proposed action ourselves (P2, P7).
		verb, arg := parseAction(reply, cfg.Tools)
		step := Step{Iter: i, Reply: reply, Verb: verb, Arg: arg, Usage: resp.Usage}

		// ---- Principle 5: a done-signal the model can emit. ----
		if verb == "answer" {
			step.Grounded = grounded
			res.Steps = append(res.Steps, step)
			// (P5/HP-5) Don't trust the done-signal blindly: re-verify the claimed
			// terminal state before accepting it. A failing check is an honest
			// non-pass, NOT a stored fact.
			if reason := verifyTermination(ctx, cfg, lastRunFailed, runTimeout); reason != "" {
				res.Outcome = Unverified
				res.Answer = arg
				res.Reason = reason
				cfg.Obs.Note("answer not verified — " + reason)
				return res, nil
			}
			res.Outcome = Answered
			res.Answer = arg
			cfg.Obs.Done(arg)
			// ---- Principles 1 & 3: persist what we concluded BEYOND this run, so
			// the next invocation starts smarter. But ONLY if the answer was
			// tool-verified this run (P4) — otherwise we risk amplifying a guess or
			// a stale recalled fact into a permanent one. ----
			if grounded {
				remember(ctx, cfg.Memory, cfg.Task, arg)
			} else if cfg.Memory != nil {
				cfg.Obs.Note("memory: answer not tool-verified this run — not stored (avoids amplifying guessed/recalled facts)")
			}
			return res, nil
		}

		// ---- Principle 5: TWO no-progress detectors. ----
		// (a) tight loop: the same action (verb+arg) repeated.
		if verb+" "+arg == lastAction {
			repeats++
			if repeats >= maxRepeats {
				res.Steps = append(res.Steps, step)
				res.Outcome = KilledRepeat
				res.Reason = fmt.Sprintf("no progress: repeated %q %d times", lastAction, repeats)
				return res, nil
			}
		} else {
			repeats = 0
		}
		lastAction = verb + " " + arg

		// (b) explore-spiral: list_dir noProgressWindow times running, even with
		// DIFFERENT args (list_dir a, b, c …) — which (a) can't see. Gated to
		// list_dir on purpose: it's the only pure-navigation tool, so grinding it
		// means wandering, not converging. read_file/run repetition is NOT a spiral
		// — a re-read/re-run of the SAME arg is (a)'s job, and DIFFERENT args
		// (paging a file, stepping a pipeline) are real progress.
		if verb == "list_dir" && verb == lastVerb {
			sameVerb++
			if sameVerb >= noProgressWindow {
				res.Steps = append(res.Steps, step)
				res.Outcome = KilledSpiral
				res.Reason = fmt.Sprintf("no progress: %d list_dir calls in a row — switch to run or read_file, or answer", sameVerb)
				return res, nil
			}
		} else {
			sameVerb = 1
		}
		lastVerb = verb

		// ACT: run the named tool. The model only chose it; we execute it (P2).
		observation := dispatch(ctx, cfg.Tools, verb, arg)

		// A successful tool observation means the model has now seen REAL external
		// state this run — anything it answers from here is grounded, so worth
		// remembering. Tool errors don't count (they aren't verified facts).
		if !strings.HasPrefix(observation, "ERROR:") {
			grounded = true
		}
		step.Grounded = grounded
		step.Observation = observation
		res.Steps = append(res.Steps, step)

		// ---- Principle 5: stagnant-observation detector. ----
		// A `run` that keeps FAILING with the byte-identical result, despite the
		// model changing actions between, is a stall the action-keyed detectors
		// above can't see. Track it on the observation, not the action.
		if verb == "run" {
			lastRunFailed = isRunFailure(observation)
			switch {
			case !lastRunFailed: // a passing run is real progress — reset.
				stagnant, lastRunFP = 0, ""
			case runFingerprint(observation) == lastRunFP:
				stagnant++
			default: // a NEW failure — the world changed, count restarts.
				stagnant, lastRunFP = 1, runFingerprint(observation)
			}
			if stagnant >= maxStagnant {
				res.Outcome = KilledStagnant
				res.Reason = fmt.Sprintf("no progress: the same command failure recurred %d times despite changing actions — the approach is stuck; change strategy or rewrite the file", stagnant)
				cfg.Obs.Observation(observation)
				return res, nil
			}
		}

		// OBSERVE: the result — including any error — is appended as the next thing
		// the model sees (P2, P4). It is REAL external state, our anchor.
		cfg.Obs.Observation(observation)
		messages = append(messages, llm.User("OBSERVATION:\n"+observation))
	}

	// ---- Principle 5: if we fall out of the loop, WE stop it. Never trust the model to. ----
	res.Outcome = HitCap
	res.Reason = fmt.Sprintf("hit iteration cap (%d) without an answer", maxIter)
	return res, nil
}

// verifyTermination decides whether a model's done-signal actually holds (P5/HP-5)
// and returns a non-empty reason when it does NOT (so the caller records Unverified
// instead of Answered). Two checks, in precedence order:
//
//   - VerifyCmd (authoritative): the caller named a success command, so re-run it.
//     A non-zero exit is ground truth — independent of what the model claimed, and
//     independent of any model cooperation. This is the precise gate that turns the
//     DOGFOOD R9/R10 false positives (a model that stops while the build is red)
//     into honest non-passes.
//   - VerifyLastRun (heuristic fallback, opt-in): with no VerifyCmd, a silent finish
//     is suspect when the most recent `run` was still failing. Weaker — a legitimate
//     absence answer can follow a non-zero exit (grep-no-match) — hence opt-in.
//
// With neither configured it returns "" (the historical behavior: trust the answer).
func verifyTermination(ctx context.Context, cfg Config, lastRunFailed bool, runTimeout time.Duration) string {
	if cfg.VerifyCmd != "" {
		out, err := runOp(ctx, cfg.Sandbox, cfg.VerifyCmd, runTimeout)
		if err != nil { // couldn't even start it — we cannot confirm success.
			return fmt.Sprintf("could not run verification command %q: %v", cfg.VerifyCmd, err)
		}
		if isRunFailure(out) {
			return fmt.Sprintf("verification command %q did not pass:\n%s", cfg.VerifyCmd, out)
		}
		return ""
	}
	if cfg.VerifyLastRun && lastRunFailed {
		return "the most recent command run was still failing and nothing succeeded after it — the task does not look complete"
	}
	return ""
}

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

// ---- small helpers ----

// addUsage sums two Usage values field-by-field so RunResult.Usage accumulates
// the whole run's token cost.
func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		CachedTokens:     a.CachedTokens + b.CachedTokens,
	}
}

// truncate is the dispatch-level BACKSTOP (P1): per-tool shaping should keep
// output well under observationCap, so this rarely fires — it just guarantees no
// single observation can blow the window.
func truncate(s string) string { return clip(s, observationCap) }

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
