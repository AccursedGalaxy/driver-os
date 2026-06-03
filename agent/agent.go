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
	"errors"
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

	// maxReasoningRepeats is the LENIENT tight-loop threshold for a turn whose
	// reasoning trace ADVANCED (a thinking model, e.g. Gemini). Such a model re-issues
	// the same visible action across turns while its (encrypted) chain of thought
	// moves forward — it uses a re-read as a "keep thinking" turn (read -> think ->
	// think -> act), so the strict maxRepeats=2 false-kills it mid-thought (the Gemini
	// regression: trace 0/5, repofix spiralling). Observed: it needs ~3-5 such turns to
	// converge, so allow more before cutting — but STILL cut, because a genuinely stuck
	// thinking model otherwise re-reads one file 20x and burns the whole budget to
	// hit_cap (a 60K-token repofix spiral). Frozen reasoning (trace unchanged) is a real
	// stall and falls back to the strict maxRepeats.
	maxReasoningRepeats = 6

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

	// searchMatchCap bounds how many matching lines `search` returns (P1, CONTEXT):
	// a common term (e.g. "err") can match thousands of lines, and dumping them all
	// would rot the window. When it clips it tells the model how to narrow (P3).
	searchMatchCap = 100

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
	Answered        Outcome = "answered"          // the model emitted `answer` (and it verified, if a check was configured).
	Unverified      Outcome = "unverified"        // the model finished, but the closing verification (VerifyCmd / last-run check) failed — a non-pass (P5/HP-5).
	HitCap          Outcome = "hit_cap"           // ran out of iterations (P5 hard cap).
	KilledRepeat    Outcome = "killed_repeat"     // exact same action maxRepeats times.
	KilledSpiral    Outcome = "killed_spiral"     // noProgressWindow list_dir calls in a row.
	KilledStagnant  Outcome = "killed_stagnant"   // the same failing `run` result maxStagnant times despite changing actions.
	HitDeadline     Outcome = "hit_deadline"      // exceeded the wall-clock budget (P5) — a spiral that dodged the action/observation detectors.
	ProviderErr     Outcome = "provider_error"    // a transport/auth failure talking to the model.
	HitContextLimit Outcome = "hit_context_limit" // (HP-1) the window overflowed AND reactive eviction couldn't compact it further — a graceful stop, not a crash.
	RefusedUnsafe   Outcome = "refused_unsafe"    // the Sandbox's isolation is weaker than Config.MinIsolation requires — refused BEFORE the first model call (P2/§5). Never ran hostile code on a too-weak boundary.
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

	// VerifySandbox is the sandbox the CLOSING gates (VerifyCmd/DiagnoseCmd) run on,
	// when it must differ from Sandbox. nil ⇒ Sandbox (the historical behavior; every
	// session-off caller, incl. all eval sweeps, leaves it nil). It exists for the
	// -session mode: the model's `run` tool acts through a STATEFUL session
	// (Sandbox), but the verification commands must run in a clean context — a model
	// that `cd`s away, or `export`s something odd, must not bend `go build ./...` or
	// the diagnostics feed. Both still hit the same warm container, so there is no
	// cache cost. See ../SESSION.md.
	VerifySandbox sandbox.Sandbox
	Tools         map[string]Tool // optional: nil = DefaultTools(Sandbox).
	Task          string          // required: the goal.
	Root          string          // optional: the dir Sandbox is rooted at; recorded in RunResult.Root.
	Obs           Observer        // optional: live progress sink; nil = silent.

	// MinIsolation is the SAFETY PRECONDITION (P2/§5): the weakest sandbox isolation
	// this run will tolerate. Before the first model call, Run/RunNative refuse with
	// Outcome RefusedUnsafe if Sandbox.Capabilities().Isolation < MinIsolation — so
	// untrusted, model-authored code never executes on a boundary too weak to
	// contain it (e.g. requiring IsolationKernel forces a gVisor backend; a `local`
	// or plain-container sandbox is refused). The default zero is IsolationNone,
	// which admits every backend and preserves today's behavior for trusted callers
	// (issue-bot, eval). This is the ONE enforcement point; CLI -untrusted just sets
	// it. Fails CLOSED: a nil Sandbox with MinIsolation > IsolationNone is a refusal,
	// not a panic.
	MinIsolation sandbox.Isolation

	// All three default sensibly when zero (P5/P7 — termination knobs are OURS):
	MaxIterations int           // 0 = DefaultMaxIterations. The hard cap on think->act->observe turns.
	MaxTokens     int           // 0 = DefaultMaxTokens. Per-turn output cap on the model call.
	RunTimeout    time.Duration // 0 = defaultRunTimeout. Wall-clock kill for a single `run` command.

	// MaxWallClock bounds the WHOLE run's wall-clock (P5), checked between turns. It
	// is the universal backstop for a spiral that dodges every action/observation
	// detector — the DOGFOOD nano case that emitted ever-changing malformed tool
	// calls (premature finishes, truncated apply_patch blobs) and was only stopped by
	// an EXTERNAL `timeout`, exiting 124 with no typed outcome. With this set the loop
	// ends itself as HitDeadline (and runs the verify-on-terminate check). Especially
	// relevant with slow third-party providers, where iteration count and wall-clock
	// diverge. 0 = off (only the iteration cap bounds the run).
	MaxWallClock time.Duration

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

	// ChurnNudgeRuns, when > 0, injects a ONE-TIME hint after this many FAILING `run`
	// results in a session: a suggestion to stop incremental editing and rewrite the
	// whole file in one write_file. It targets the residual cheap-model failure the
	// live runs surfaced — a capable model (gpt-oss-120b, grok) that passes when it
	// writes the file wholesale but burns to the iteration cap when it wanders in
	// edit_file/run churn on shifting line numbers. The runs that rewrite pass; the
	// runs that churn time out, so nudging a stuck model toward a rewrite is the lever
	// to consistent passing. 0 = off (the historical behavior).
	ChurnNudgeRuns int

	// VerifyContinue turns the VerifyCmd gate from TERMINAL into CONTINUE-ON-FAIL: a
	// finish that doesn't verify is not accepted as Unverified while iterations
	// remain — instead the failing output is fed back as an observation and the loop
	// keeps going. This is the lever that lifts weak-model pass rates: the dominant
	// cheap-model failure (DOGFOOD R9/R10) is a PREMATURE finish — a tool-call-free
	// turn that is really narration ("I'll implement now") or a hallucinated "done",
	// emitted before the work is complete. Re-grounding it with the real red test
	// output and "keep working" converts that false stop into actual progress, where
	// the plain terminal gate would just record a non-pass. Bounded by MaxIterations;
	// requires VerifyCmd.
	VerifyContinue bool

	// FinishNudgeWindow arms HP-4's near-cap FINISHER. When > 0, and the run is
	// within this many turns of the iteration cap, AND the world looks SETTLED — the
	// most recent `run` exited 0 (build/test green) and no file has been mutated
	// (write_file/edit_file) for this many turns — a one-time hint is injected telling
	// the model the task appears complete and to finish now (or say what remains). It
	// manufactures the finish ATTEMPT the spinners never make: the gemini/grok runs
	// that burn to the cap on ALREADY-GREEN code (hit_cap-but-passing), which
	// upgradeIfVerified only rescues at the very end. The nudge is SAFE: the resulting
	// finish still routes through verifyTermination, so a premature/false "done" is
	// caught (and under VerifyContinue becomes more work, not a stop). Gated on a green
	// `run` on purpose — with no build/test executed there is no grounded done-signal,
	// so the finisher stays out and the cap/other detectors bound those runs. 0 = off.
	//
	// Eval-validated (selfhist, gemini-3.1-pro, window=3): it converts hit_cap-but-passing
	// into clean `answered` (fired on the 2 settled trials, stayed out on the red-build
	// one — 0 false-positives). NOTE the window governs WHAT it buys: a small window
	// fires LATE (iter 27–29 of 30), so it cleans the OUTCOME SIGNAL but does not cut
	// tokens — the run still reaches the cap. To cut spend, widen the window so it fires
	// earlier (e.g. 8–10), trading against interrupting a model still doing real work.
	FinishNudgeWindow int

	// DiagnoseCmd arms slice 1 of the code-intelligence work (see CODE-INTELLIGENCE.md):
	// a fast compile/type-check command (e.g. "go build ./...") run as a diagnostics
	// SOURCE when the model looks stuck with a broken build, whose errors are surfaced
	// into the loop as INFORMATION — never a gate (the gate stays at termination,
	// VerifyCmd). It targets the dominant build-broken failure: a model that edits
	// without self-checking and never learns it left a compile error (the glm-5 `"errors"
	// imported and not used` → hit_cap case). The source/surfacing split is deliberate —
	// a future persistent-gopls client becomes an alternative SOURCE behind this same
	// call, leaving the loops' SURFACING untouched, so only this command changes. Empty =
	// off. May name the same command as VerifyCmd or a cheaper one (build vs. test).
	DiagnoseCmd string

	// DiagnoseAfterEdits is the stuck threshold for DiagnoseCmd: the feed stays silent
	// until the model has made this many file edits WITHOUT reaching a green build/run in
	// between (the counter resets on any passing `run` or a clean DiagnoseCmd). The
	// threshold is what honors the multi-file reality — a multi-file change is legitimately
	// red mid-flight, so the feed must NOT nag after the first edit, only once the model is
	// clearly not converging. 0 = off (DiagnoseCmd is also required to arm).
	DiagnoseAfterEdits int
}

// Run is the entire agent. Notice it is tiny — the loop is trivial (P3); the
// interesting work is the context policy and the termination conditions. It
// prints nothing: events go to cfg.Obs, the terminal state to the returned
// RunResult. err is non-nil ONLY for a genuine infrastructure failure (the model
// call itself failed); a no-progress kill or a hit cap is a normal Outcome, not
// a Go error.
// checkIsolation enforces Config.MinIsolation (P2/§5) and is the FIRST thing both
// loops do. It returns a non-nil RefusedUnsafe RunResult — to be returned
// directly, before any model call or verification — when the Sandbox's isolation
// is weaker than required, and nil when the run may proceed. It fails CLOSED: a
// nil Sandbox under a non-zero MinIsolation is a refusal, not a panic. The caller
// must return this result AS-IS and must NOT route it through upgradeIfVerified —
// a refused run must never execute VerifyCmd on the unsafe sandbox.
func checkIsolation(cfg Config) *RunResult {
	if cfg.MinIsolation <= sandbox.IsolationNone {
		return nil // default: every backend admitted; today's trusted-caller behavior.
	}
	have := sandbox.IsolationNone
	if cfg.Sandbox != nil {
		have = cfg.Sandbox.Capabilities().Isolation
	}
	if have < cfg.MinIsolation {
		return &RunResult{
			Task:    cfg.Task,
			Root:    cfg.Root,
			Outcome: RefusedUnsafe,
			Reason: fmt.Sprintf("refused: task requires isolation >= %s but the sandbox provides %s — "+
				"run with a stronger backend (e.g. -sandbox=docker -runtime=runsc)", cfg.MinIsolation, have),
		}
	}
	return nil
}

func Run(ctx context.Context, cfg Config) (*RunResult, error) {
	if refusal := checkIsolation(cfg); refusal != nil {
		return refusal, nil // (P2/§5) too-weak sandbox — refuse before the first model call.
	}
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
	failRuns := 0   // count of failing `run` results this session (a churn signal).
	edits := 0      // count of edit_file calls this session (the other churn signal).
	nudged := false // the churn nudge fires at most once.

	// (code-intel slice 1) edits since the last green build/run — the diagnostics-feed
	// stuck signal (reset on any passing `run` or a clean DiagnoseCmd). See DiagnoseCmd.
	editsSinceGreen := 0

	// (HP-4) Near-cap finisher state. lastRunPassed is the "build/test green" half of
	// the done-signal (the most recent `run` exited 0); lastEditIter is the iteration
	// of the most recent file mutation (0 = none yet), so i-lastEditIter measures how
	// long the files have been stable; finishNudged fires the hint at most once.
	lastRunPassed := false
	lastEditIter := 0
	finishNudged := false

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

	for i := 1; i <= maxIter; i++ { // (P5) the hard cap lives in the loop header.
		if cfg.MaxWallClock > 0 && time.Since(start) > cfg.MaxWallClock {
			res.Outcome = HitDeadline
			res.Reason = fmt.Sprintf("hit wall-clock budget (%s) after %d turn(s)", cfg.MaxWallClock, i-1)
			return upgradeIfVerified(cfg, res, runTimeout), nil
		}
		cfg.Obs.Iteration(i, maxIter)

		// THINK: send the FULL context (P1) and get back text. Pure function.
		// generateWithEviction adds HP-1's reactive fallback: on a window overflow
		// it compacts the OLDEST turn and retries instead of crashing, returning the
		// possibly-shrunk transcript we carry forward.
		var resp *llm.Response
		var err error
		resp, messages, err = generateWithEviction(loopCtx, cfg, llm.Request{
			System:      system,
			Messages:    messages,
			Temperature: &temp,
			MaxTokens:   maxTok, // (P7) our cap, resolved from Config. Too low silently clips a long answer/edit.
		})
		if err != nil {
			// A deadline hit mid-Generate is the wall-clock budget, not a transport fault.
			if cfg.MaxWallClock > 0 && loopCtx.Err() == context.DeadlineExceeded {
				res.Outcome = HitDeadline
				res.Reason = fmt.Sprintf("hit wall-clock budget (%s) mid-turn", cfg.MaxWallClock)
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
			// (HP-1) The window overflowed and eviction couldn't compact it any
			// further (only TASK + the most recent turn remain). Degrade gracefully
			// to a typed stop rather than surfacing it as a transport fault.
			if errors.Is(err, llm.ErrContextLength) {
				res.Outcome = HitContextLimit
				res.Reason = "context window exceeded and could not be compacted further"
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
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
			// terminal state before accepting it.
			reason := verifyTermination(ctx, cfg, lastRunFailed, runTimeout)
			if reason != "" && cfg.VerifyContinue && i < maxIter {
				// Continue-on-fail: a premature finish becomes more work, not a stop.
				// Feed the real failing state back (P4) and keep going.
				cfg.Obs.Note("finish rejected (not verified) — continuing")
				messages = append(messages, llm.User("OBSERVATION:\nNot finished — you answered, but the task is not verified:\n"+reason+"\nKeep working: fix the code and re-run until it passes."))
				continue
			}
			if reason != "" {
				// No budget left (or not in continue-mode): an honest non-pass, not a stored fact.
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
				return upgradeIfVerified(cfg, res, runTimeout), nil
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
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
		} else {
			sameVerb = 1
		}
		lastVerb = verb

		// ACT: run the named tool. The model only chose it; we execute it (P2).
		observation := dispatch(loopCtx, cfg.Tools, verb, arg)

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
			lastRunPassed = isRunSuccess(observation) // (HP-4) the green/red of the most recent run.
			if lastRunPassed {
				editsSinceGreen = 0 // (code-intel slice 1) reaching green clears the diagnostics-stuck count.
			}
			if lastRunFailed {
				failRuns++
			}
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
				return upgradeIfVerified(cfg, res, runTimeout), nil
			}
		}
		if verb == "edit_file" {
			edits++
		}
		if verb == "write_file" || verb == "edit_file" {
			lastEditIter = i // (HP-4) a file mutation resets the "files stable" clock.
			editsSinceGreen++

			// (code-intel slice 1) Diagnostics feed: once the model has edited
			// DiagnoseAfterEdits times WITHOUT reaching a green build, run the diagnostics
			// SOURCE and surface its errors as INFORMATION (not a gate; see
			// CODE-INTELLIGENCE.md). A clean build means it actually reached green via the
			// check, so reset and stay silent. Only on edit turns — no point re-checking a
			// read/run turn. The threshold keeps it out of a legitimately-red multi-file
			// change until the model is clearly not converging.
			if cfg.DiagnoseCmd != "" && cfg.DiagnoseAfterEdits > 0 && editsSinceGreen >= cfg.DiagnoseAfterEdits {
				if report, clean := diagnoseSource(loopCtx, cfg, runTimeout); clean {
					editsSinceGreen = 0
				} else {
					observation += "\n\n" + diagnosticsMessage(cfg.DiagnoseCmd, report)
					cfg.Obs.Note("stuck with a broken build — surfacing diagnostics (code-intel slice 1)")
				}
			}
		}

		// (P3) Churn nudge: fire once when EITHER signal of wandering crosses the
		// threshold — repeated failing test-runs (the gpt-oss churn) OR many
		// edit_file calls without converging (the grok read/edit churn, which barely
		// runs the tests so a run-only trigger never fires). Appended to whatever the
		// current observation is so the model reads it next turn.
		if !nudged && cfg.ChurnNudgeRuns > 0 && (failRuns >= cfg.ChurnNudgeRuns || edits >= cfg.ChurnNudgeRuns) {
			observation += churnNudge
			nudged = true
		}

		// (HP-4) Near-cap finisher: when the budget is nearly spent AND the world looks
		// settled — the last `run` was green and the files have been stable for the
		// window — manufacture the finish ATTEMPT the spinner never makes. Appended to
		// THIS observation so the model reads it next turn. One-time; the i < maxIter
		// guard guarantees the model gets at least one more turn to act on it (a hint on
		// the very last turn would be wasted). See Config.FinishNudgeWindow.
		if !finishNudged && cfg.FinishNudgeWindow > 0 && i < maxIter &&
			maxIter-i <= cfg.FinishNudgeWindow &&
			lastRunPassed && i-lastEditIter >= cfg.FinishNudgeWindow {
			observation += finishNudgeText
			finishNudged = true
			cfg.Obs.Note("near cap with a green build and stable files — nudging to finish (HP-4)")
		}

		// OBSERVE: the result — including any error — is appended as the next thing
		// the model sees (P2, P4). It is REAL external state, our anchor.
		cfg.Obs.Observation(observation)
		messages = append(messages, llm.User("OBSERVATION:\n"+observation))
	}

	// ---- Principle 5: if we fall out of the loop, WE stop it. Never trust the model to. ----
	res.Outcome = HitCap
	res.Reason = fmt.Sprintf("hit iteration cap (%d) without an answer", maxIter)
	return upgradeIfVerified(cfg, res, runTimeout), nil
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

// diagnoseSource runs cfg.DiagnoseCmd as slice 1's diagnostics SOURCE (a fast
// compile/type check — go build/go vet via the sandbox) and reports its output and
// whether the build is clean (exit 0). The source/surfacing split (see
// CODE-INTELLIGENCE.md) keeps this single call as the seam a future persistent-gopls
// client slots behind, leaving the loops' surfacing logic untouched. A "" command, an
// infra failure to even start it, or a clean build all yield clean=true and no report
// — we never fabricate a build error out of an infrastructure fault (P6).
// verifySandbox is the sandbox the closing verification/diagnostics commands run
// on: VerifySandbox when set, else Sandbox. The split lets -session route the
// model's `run` tool through a stateful session while these trust-critical gates
// run in a clean context (see Config.VerifySandbox / ../SESSION.md).
func (c Config) verifySandbox() sandbox.Sandbox {
	if c.VerifySandbox != nil {
		return c.VerifySandbox
	}
	return c.Sandbox
}

func diagnoseSource(ctx context.Context, cfg Config, timeout time.Duration) (report string, clean bool) {
	if cfg.DiagnoseCmd == "" {
		return "", true
	}
	out, err := runOp(ctx, cfg.verifySandbox(), cfg.DiagnoseCmd, timeout)
	if err != nil || !isRunFailure(out) {
		return "", true
	}
	return out, false
}

// diagnosticsMessage renders the stuck-with-a-broken-build feed: the failing command
// and its (already clipped by formatRun) output, framed explicitly as INFORMATION, not
// a stop. Shared by both loops so the wording is identical regardless of protocol.
func diagnosticsMessage(cmd, report string) string {
	return fmt.Sprintf("[diagnostics] your recent edits do not build yet — `%s` reports:\n%s\n"+
		"Fix these errors and continue. This is informational, not a stop.", cmd, report)
}

// upgradeIfVerified flips a non-Answered terminal outcome (a kill or the iteration
// cap) to Answered when VerifyCmd actually passes (P5/HP-5). The verify command is
// the source of truth for "is the task done", so a run whose code is already correct
// must not be reported as a failure just because the loop bailed — the live runs
// showed gpt-5-nano flail on a malformed `bash -lc go test` side-command (tripping
// the stagnant detector) while its real calc.go passed. This is the dual of
// verify-continue: that downgrades a false success, this upgrades a false failure.
// No-op without VerifyCmd, or when the command still fails (a genuine non-pass).
func upgradeIfVerified(cfg Config, res *RunResult, runTimeout time.Duration) *RunResult {
	if cfg.VerifyCmd == "" {
		return res
	}
	// A FRESH context, detached from the loop's: this final check must run even when
	// the run ended because the wall-clock budget (loop ctx) just expired — otherwise
	// a HitDeadline on already-correct code could never be upgraded. Bounded by runOp's
	// own timeout so it can't itself hang.
	out, err := runOp(context.Background(), cfg.verifySandbox(), cfg.VerifyCmd, runTimeout)
	if err == nil && !isRunFailure(out) {
		res.Reason = fmt.Sprintf("completed despite %s — %q passed", res.Outcome, cfg.VerifyCmd)
		res.Outcome = Answered
		if res.Answer == "" {
			res.Answer = fmt.Sprintf("task verified complete (%q passed)", cfg.VerifyCmd)
		}
	}
	return res
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
		out, err := runOp(ctx, cfg.verifySandbox(), cfg.VerifyCmd, runTimeout)
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

// generateWithEviction sends the FULL context (P1) and applies HP-1's REACTIVE
// fallback on a context-window overflow: rather than crash the run, it evicts the
// oldest tool turn (pairing-safe — see evictOldestTurn) and retries. It returns
// the possibly-shrunk message slice so the caller carries the compaction forward
// into later turns (not just this one). This is deliberately NOT a context POLICY
// (the policy question is HP-1, parked: we're nowhere near the window on real
// tasks — measured ≤10k tokens of a 200k+ window). It is the cheap safety net
// that turns a latent hard crash into graceful degradation IF a transcript ever
// does overflow. A non-overflow error is returned untouched; an overflow that
// can't be compacted any further is returned AS llm.ErrContextLength so the
// caller degrades to HitContextLimit instead of mislabelling it a transport fault.
func generateWithEviction(ctx context.Context, cfg Config, req llm.Request) (*llm.Response, []llm.Message, error) {
	for {
		resp, err := cfg.Model.Generate(ctx, req)
		if err == nil || !errors.Is(err, llm.ErrContextLength) {
			return resp, req.Messages, err
		}
		shrunk, ok := evictOldestTurn(req.Messages)
		if !ok {
			return nil, req.Messages, err // can't compact further — let the caller degrade.
		}
		cfg.Obs.Note(fmt.Sprintf("context overflow — evicted oldest turn (%d→%d msgs), retrying (HP-1 reactive fallback)", len(req.Messages), len(shrunk)))
		req.Messages = shrunk
	}
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
