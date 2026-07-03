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
	"iter"
	"math"
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

	// UncappedIterations is a MaxIterations sentinel meaning "effectively no cap" —
	// for interactive front-ends where a human watches the loop and can interrupt.
	// It is a large positive value so it flows through the loop's `maxIter <= 0 ->
	// DefaultMaxIterations` fallback unchanged.
	UncappedIterations = math.MaxInt32
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
	//
	// KNOWN TRADEOFF (review #12): for a reasoning provider whose opaque trace
	// moves EVERY turn (Gemini via OpenRouter re-encrypts its thought signature
	// each call), reasoningAdvanced is ~always true — so for those providers the
	// strict thresholds (maxRepeats, the strict spiral window) are effectively
	// INERT and THESE lenient ones (this constant; 2× the spiral window) are the
	// real operating bounds. Tuning the strict constants will not change thinking-
	// model behavior; tune the lenient ones. This is deliberate: every cheaper
	// secondary gate tried so far false-killed real work (the N3 reasoning-token
	// gate regressed the gemini trace eval 5/5→0/5 and was reverted, pinned by
	// TestRunNativeReasoningAdvanceEscapesRepeatDetector and kin). If a genuinely-
	// stuck thinking model burning the 6×/8× budget ever shows up in a trace, the
	// candidate secondary signal is "did the OBSERVATION change too" — design it
	// from that failing trace, not from here.
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
	KilledSpiral    Outcome = "killed_spiral"     // noProgressWindow discovery-only turns (list_dir/search) in a row.
	KilledStagnant  Outcome = "killed_stagnant"   // the same failing `run` result maxStagnant times despite changing actions.
	HitDeadline     Outcome = "hit_deadline"      // exceeded the wall-clock budget (P5) — a spiral that dodged the action/observation detectors.
	HitBudget       Outcome = "hit_budget"        // exceeded the token budget (P5/HP-8, MaxTotalTokens) — cost is a first-class cap, not a side effect of the iteration count.
	ProviderErr     Outcome = "provider_error"    // a transport/auth failure talking to the model.
	HitContextLimit Outcome = "hit_context_limit" // (HP-1) the window overflowed AND reactive eviction couldn't compact it further — a graceful stop, not a crash.
	RefusedUnsafe   Outcome = "refused_unsafe"    // the Sandbox's isolation is weaker than Config.MinIsolation requires — refused BEFORE the first model call (P2/§5). Never ran hostile code on a too-weak boundary.
	ScopeViolation  Outcome = "scope_violation"   // the run's diff escaped the configured writable globs (Config.DiffScope) — a changed file was outside the declared task scope, e.g. rewriting production code to fit a guard test.
	Canceled        Outcome = "canceled"          // the caller canceled the run (SIGINT / ctx cancel) — not a provider fault (distinct from HitDeadline, which is the run's own wall-clock budget).
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
	// FinishReason is why THIS turn's generation stopped — a turn property
	// (every step of a multi-call turn carries it, like ReasoningAdvanced).
	// The post-mortem discriminator for a mid-sentence answer: "stop" = the
	// model chose to end there, "length" = token cap, "" on a streamed call =
	// the stream ended with NO terminal chunk (silent EOF — likely truncated
	// upstream).
	FinishReason llm.FinishReason
	// ModelMs/ToolMs split the turn's wall-clock so provider latency and tool
	// execution can be told apart (a 25s `run` vs a 25s slow model look identical
	// in iteration counts). ToolMs is 0 on the answer turn (no tool dispatched).
	ModelMs int64
	ToolMs  int64
	// ReasoningAdvanced is true when this turn's (opaque) provider reasoning trace
	// DIFFERED from the previous turn's — a thinking model still moving even if its
	// visible action repeats. It selects the lenient tight-loop/spiral thresholds;
	// see maxReasoningRepeats. Deliberately independent of Usage.ReasoningTokens:
	// gemini moves its encrypted thought-signature while reporting zero tokens, and
	// that movement is real thought (a token gate was tried and reverted 2026-06-12
	// after regressing the trace eval 5/5 → 0/5; see loop_tools.go).
	ReasoningAdvanced bool
}

// RunResult is the structured outcome of a Run: the typed Outcome, the answer
// (if any), the full Step trace, summed token Usage, and the Root the sandbox
// ran against. Root is the fixture hook (HP-11): a baseline diff must know
// whether a case ran against an immutable fixture or the live repo, so the
// working dir travels with the result instead of being implicit.
type RunResult struct {
	// ID is a stable, time-sortable identifier for this run ("<YYYYMMDD-HHMMSS>-<hex>"),
	// stamped by Run/RunNative. It is the spine: a transcript, an eval Trial, a council
	// AgentTrace, and a commit-msg dogfood record can all reference the SAME run by ID
	// instead of each re-embedding their own copy.
	ID string
	// StartedAt/EndedAt bound the run's wall-clock (stamped by the loop). Distinct from
	// the per-step ModelMs/ToolMs, which sum only time spent IN model calls and tools.
	StartedAt  time.Time
	EndedAt    time.Time
	Task       string
	Root       string  // the dir the sandbox was rooted at (Config.Root).
	Outcome    Outcome // how the run ended.
	Answer     string  // the final answer, set iff Outcome == Answered.
	Reason     string  // human explanation for a non-Answered outcome (kept so the CLI prints the old message verbatim).
	Steps      []Step  // the full trace.
	Iterations int     // turns taken.
	Usage      llm.Usage
	Err        error // set iff Outcome == ProviderErr.
	// Review is the review-gate record — rounds, every finding with its fate,
	// and the reviewer's token cost (the calibration telemetry). nil when no
	// Reviewer was configured; populated on every loop exit when one was, even
	// if the gate never fired (Skipped says why).
	Review *ReviewReport
	// Plan is the plan-stage record — the plan the solver was handed (or why
	// there was none) and the planner's own token cost, kept OUT of Usage so
	// per-role spend stays attributable. nil when no Planner was configured.
	Plan *PlanReport
	// Messages is the FULL conversation as it stood when the run ended — the
	// system-framed TASK (or the seeded History plus this turn's input), every
	// assistant turn, and every tool result. It is the continuation seam: a chat
	// front-end feeds it back as the next run's Config.History so the model sees
	// the whole prior conversation (see Session). Populated on every path that
	// reaches the loop; nil for a pre-loop refusal (too-weak sandbox).
	Messages []llm.Message

	// VerifyBaselineRed is true when VerifyCmd was configured and it already
	// failed on the untouched workspace before the first model call.
	VerifyBaselineRed bool `json:"verify_baseline_red,omitempty"`
	// VerifyBaselineOut is the failing output of the baseline VerifyCmd run,
	// clipped like other observations. Empty when the baseline was green.
	VerifyBaselineOut string `json:"verify_baseline_out,omitempty"`

	// memDone closes when the post-answer memory store finishes; nil when no
	// store was started. Unexported (a process handle, not run data) — await it
	// through AwaitMemory.
	memDone <-chan struct{}
}

// AwaitMemory blocks until the background memory store started for this run's
// answer (if any) has completed. The loops fire the store asynchronously so the
// result reaches the caller immediately (review #4); a caller that is about to
// EXIT the process must await it, or the extracted facts die with the process.
// Long-lived callers (a REPL, duet) can ignore it — the store finishes on its
// own. Returns immediately when no store was started; nil-safe, so a caller can
// await its "last result" without guarding the no-turns-yet case.
func (r *RunResult) AwaitMemory() {
	if r != nil && r.memDone != nil {
		<-r.memDone
	}
}

// Config is everything Run needs. Model, Sandbox, and Task are required; the
// rest default sensibly (nil Tools -> DefaultTools, nil Memory -> no cross-run
// recall, nil Obs -> silent).
type Config struct {
	Model   llm.Provider    // required: the (context) -> text engine.
	Sandbox sandbox.Sandbox // required: the isolation boundary every effect flows through (P2).
	Memory  mneme.Memory    // optional: cross-run long-term memory; nil = stateless.

	// Persona is an optional identity block prepended to the system prompt — a
	// stable character the agent keeps across runs (e.g. "You are Adam, an
	// energetic builder…"). Empty = the bare tool-using harness prompt. It leads
	// the prompt so identity frames the tool instructions; the task and recalled
	// memory still follow. Used by multi-agent callers (see ../duet) to give two
	// agents distinct voices on top of the same loop.
	Persona string

	// MemoryScope namespaces this run's long-term memory (mneme isolates facts per
	// scope on both write and recall). The zero value falls back to the package
	// default scope, preserving single-agent behavior. Set it to a per-agent scope
	// (e.g. {AgentID: "adam"}) so several agents can share ONE store without their
	// memories bleeding into each other.
	MemoryScope mneme.Scope

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
	Task          string          // required: the goal (this turn's user input when continuing a conversation).
	TaskImages    []llm.ImagePart // optional image parts for THIS turn's user message; cfg.Task stays the text projection used for recall/memory/plan/RunResult.
	// History is a prior conversation to CONTINUE from (the continuation seam, see
	// Session). When non-empty, the loop seeds its message slice with these and
	// appends Task as the next user turn — so the model sees the whole prior
	// exchange instead of a fresh "TASK:" framing. Empty (the default) is the
	// historical single-shot behavior: the run starts from "TASK: " + Task. The
	// loop clones it before appending, so the caller's slice is never mutated.
	History []llm.Message
	Root    string   // optional: the dir Sandbox is rooted at; recorded in RunResult.Root.
	Obs     Observer // optional: live progress sink; nil = silent.

	// Stream opts this run into token streaming: when set AND the provider reports
	// Capabilities().Streaming, each model call goes through Provider.Stream and the
	// incremental text deltas are pushed to Obs if it implements DeltaObserver — the
	// Claude-Code-style live-typing feel a chat front-end wants. Off by default, so
	// every existing caller (eval, issue-bot, council) keeps the single Generate call
	// and byte-identical behavior. A non-streaming provider silently uses Generate
	// even when this is set. Adapters yield the turn's reasoning trace as an
	// assembled Reasoning chunk (llm.Chunk) and collectStream keeps it, so a
	// streamed turn replays its trace — and sees the reasoning-aware no-progress
	// window — the same as a Generate turn.
	Stream bool

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

	// VerifyTimeout bounds the closing VerifyCmd executions (final gate,
	// pre-flight baseline, kill/cap upgrade check) separately from the
	// per-`run`-tool RunTimeout. 0 = max(resolved RunTimeout, 5 minutes):
	// a verify suite is routinely slower than a single interactive command,
	// and a too-short bound turns "couldn't finish checking" into a false
	// "did not pass".
	VerifyTimeout time.Duration

	// ReasoningEffort rides on every model call of the run (llm.Request
	// passthrough: "minimal".."xhigh", "" = provider default). It is a QUALITY
	// knob, not a termination knob — reasoning tokens still bill against
	// MaxTokens and MaxTotalTokens, so a higher effort spends more of both.
	ReasoningEffort string

	// PromptProfile selects the NATIVE loop's base system prompt (PROMPT-SKILLS
	// slice 2). "" or "legacy" = the historical four-sentence prompt (the
	// default, so every existing caller is byte-identical); "structured" = the
	// sectioned working-rules prompt measured against it. It is an independent
	// switch — not derived from the model — precisely so an A/B arm differs in
	// this field alone and acceptance/rollback is one flag flip. An unknown
	// value fails CLOSED before the first model call: a typo must not silently
	// run the wrong arm of a paid experiment. The text loop ignores it
	// (Run/buildSystemPrompt is protocol-shaped, not profile-shaped).
	PromptProfile string

	// MaxWallClock bounds the WHOLE run's wall-clock (P5), checked between turns. It
	// is the universal backstop for a spiral that dodges every action/observation
	// detector — the DOGFOOD nano case that emitted ever-changing malformed tool
	// calls (premature finishes, truncated apply_patch blobs) and was only stopped by
	// an EXTERNAL `timeout`, exiting 124 with no typed outcome. With this set the loop
	// ends itself as HitDeadline (and runs the verify-on-terminate check). Especially
	// relevant with slow third-party providers, where iteration count and wall-clock
	// diverge. 0 = off (only the iteration cap bounds the run).
	MaxWallClock time.Duration

	// MaxTotalTokens bounds the run's CUMULATIVE token spend (prompt + completion,
	// summed across turns — RunResult.Usage.TotalTokens), checked at the turn
	// boundary like MaxWallClock. It is the cost cap the iteration cap only
	// approximates: with full-window re-send the prompt grows ~quadratically
	// (HP-8), so a spiral's later turns are far more expensive than its early
	// ones and N iterations can cost 10× what N/2 did. The turn that crosses the
	// budget still gets processed (it may BE the answer); the loop then ends as
	// HitBudget — which the closing verification can still upgrade, exactly like
	// a cap/deadline exit. Per-turn cumulative usage flows through Obs.Note so a
	// caller can measure before choosing a number. 0 = off.
	MaxTotalTokens int

	// VerifyCmd is the closing VERIFICATION gate (P5/HP-5): a success command the
	// caller names (e.g. "go test ./...") that is re-run when the model finishes.
	// A non-zero exit downgrades the terminal Answered to Unverified — turning a
	// model that stopped while the task was still broken (DOGFOOD R9/R10's
	// termination-by-silence false positives: narrated intent, acknowledged
	// failure, hallucinated success) into an honest non-pass instead of exit-0
	// success. Empty = no closing check. The harness does NOT guess the success
	// criterion; the caller states it.
	VerifyCmd string

	// SkipVerifyBaseline, when true, opts out of the pre-flight verification
	// check. By default (false), when VerifyCmd is set, the harness runs it
	// once on the untouched workspace BEFORE the first model call to record
	// whether the gate starts red.
	SkipVerifyBaseline bool

	// AbortOnRedBaseline, when true AND the pre-flight baseline measures red,
	// causes BOTH loops to return immediately before the first model call with
	// Outcome Unverified, Iterations 0, and a Reason stating the verify command
	// was already failing on the untouched workspace. The caller sets this when
	// a red baseline means the gate is unsatisfiable and the run should not
	// spend any budget. Requires VerifyCmd; inert when SkipVerifyBaseline is
	// also set (skip wins — baseline is not measured, so there is no signal to
	// abort on).
	AbortOnRedBaseline bool

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

	// TestFence is the READ-ONLY glob list for the run (REVIEW-GATE slice 0;
	// recommended `*_test.go,testdata/**`): write_file/edit_file refuse matching
	// paths outright, every fenced file is hashed at run start, and ANY drift at
	// a closing gate — including a `run`-mediated shell redirect — makes the run
	// Unverified with the files named. It closes the hole the probe only papered
	// over with prompt text ("do not modify tests"): test-file immutability must
	// be enforced by the HARNESS, not asked of the model. Empty = off (today's
	// behavior byte-for-byte); opt-in for now — eval/challenge runs and -review
	// runs set it.
	TestFence []string

	// DiffScope is the WRITABLE allowlist for the run (the inverse of TestFence):
	// a list of path globs the solver's changes MAY touch; any change outside it
	// is a first-class failure (ScopeViolation), not a pass.
	//
	// Scope globs are anchored at the repository ROOT:
	//   - `dir/**`  matches paths that start with `dir/` (and `dir` itself).
	//     It does NOT match `pkg/dir/evil.go` — the scope is a prefix, not a
	//     substring.
	//   - `*.go`    (bare, no slash) matches only root-level files whose name
	//     matches the pattern.  It does NOT match `pkg/x.go`.
	//   - `ci/build.sh`, `.github/*` (slash present) — exact path.Match on the
	//     full root-relative path.
	//
	// Enforcement is layered, mirroring the test fence:
	//   1. Tool layer: write_file/edit_file (and append) REFUSE a path outside
	//      the scope with a recovery-shaped error — the cheap, immediate fence.
	//   2. Closing gate: a git-tree snapshot pair (run-start → gate-time) catches
	//      changes that went AROUND the tools (shell redirects, sed -i, git
	//      checkout, build artifacts); the run terminates ScopeViolation.
	//   3. Degrade loudly: if the run-start snapshot fails (non-git workspace),
	//      a Note is recorded and the tool-layer enforcement alone is active —
	//      a violation is never fabricated from an infrastructure fault.
	//
	// When BOTH TestFence and DiffScope are set: the fence WINS for fenced
	// paths (they are read-only even when in scope); a refusal message names
	// whichever mechanism refused.
	//
	// Empty = off (today's behavior byte-for-byte). Motivation: a solver asked
	// to add a guard test reordered production code the test was meant to pin,
	// to make its own test pass — reward-hacking the gate.
	DiffScope []string

	// Reviewer arms the REVIEW GATE (REVIEW-GATE slice 1): an injected,
	// independent model reviewer run at every path that can end Answered, ONLY
	// after the fence and VerifyCmd pass (execution-first). Blocking findings —
	// grounded by verbatim quote, over the confidence gate or confirmed by an
	// executed repro — are fed back to the solver VerifyContinue-style while
	// ReviewRounds remain, then mark the run Unverified. nil = gate off.
	// Implementations live outside agent (council.CodeReviewer) — council
	// imports agent, so the reviewer must be injected to avoid the cycle.
	Reviewer Reviewer

	// ReviewRounds caps the reviewer↔solver repair cycles (0 =
	// DefaultReviewRounds). Bounded on purpose: refine-loop gains concentrate in
	// round 1, and unbounded review loops flip correct patches to wrong.
	ReviewRounds int

	// Planner arms the PLAN STAGE (triad slice 3, plan.go): an injected,
	// independent planner model run ONCE before the first solver turn, whose
	// plan is appended to the seeded task. Fails OPEN — a planner error never
	// blocks the run. nil = stage off. Implementations live outside agent
	// (council.Planner) — same injection pattern as Reviewer.
	Planner Planner

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

	// DiagnoseCmd arms slice 1 of the code-intelligence work (see docs/specs/CODE-INTELLIGENCE.md):
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

	// NavSpiralWindow overrides the explore-spiral detector's threshold — the number
	// of consecutive discovery-only turns (list_dir/search, even with different args)
	// that ends the run as KilledSpiral. 0 = the default noProgressWindow. A turn
	// whose reasoning trace advanced gets 2× this window (the measured glm-5
	// orientation-burst false-kill — see the detector comment). It exists
	// for the OBSERVE-only agent whose whole job is to survey a tree: a read-only
	// critic legitimately does a top-down `list_dir .`, `cmd`, `internal`, `pkg` sweep
	// (or a burst of `search`es) before reading, which is several discovery turns in a
	// row and would trip the default detector before it critiques anything (council
	// code mode, docs/specs/COUNCIL.md slice 4 / objection O7). Raising it for that caller is an
	// OPT-IN relaxation — every other caller (issue-bot, eval, plan mode) leaves it 0
	// and keeps the strict default, so the harness is not weakened.
	NavSpiralWindow int

	// AnswerNudgeWindow arms a near-cap answer-forcer (native loop) for an OBSERVE-ONLY
	// agent: when > 0 and the run is within this many turns of the iteration cap, a
	// one-time hint tells the model to stop using tools and give its final answer NOW.
	// It is the observe-only sibling of FinishNudgeWindow: that finisher is gated on a
	// green `run` and stable files, which a read-only agent (no `run`, no edits) never
	// has — so it can never fire for a critic. DOGFOOD (council slice 4): a code critic
	// over a repo issued a read_file/search every single turn and NEVER emitted a
	// no-tool-call answer turn, hitting the cap with zero output on every budget tried —
	// the native loop only terminates on a text answer the model wouldn't produce on its
	// own. This nudge manufactures the answer attempt. It fires ONLY when the toolset is
	// observe-only (isObserveOnly — an allowlist of read-only built-ins, fail-closed);
	// an effectful/coding run leaves it inert and uses FinishNudgeWindow instead, so a
	// nudged premature "done" can never mask unverified broken work. 0 = off.
	AnswerNudgeWindow int

	// FinishTool names a first-class TERMINAL tool (native loop only): when the
	// model calls a tool with this name, the turn ends cleanly as Answered with that
	// call's "message" field as RunResult.Answer. It is the structured counterpart to
	// the prose-termination done-signal (stop calling tools, reply in text) — for a
	// caller whose whole turn IS "send a message" (duet's `say`), giving the model a
	// real finish ACTION beats steering it toward "reply with no tool", which cheap
	// models fight (they call a nonexistent `answer` tool or run `answer` as a shell
	// command and burn the turn, then idle to the cap: DUET-DOGFOOD F1/N1/N2). Any
	// non-finish calls in the same turn run first (a final cp/build is a legit last
	// action). An explicit finish is a STRONGER done-signal than silence, so it skips
	// the silence-reverification heuristic. Empty = no terminal tool (the default;
	// every existing caller terminates on the prose path, unchanged). The named tool
	// must still be present in Tools so it's advertised to the model.
	FinishTool string

	// FinishToolTrustsCaller opts a FinishTool call OUT of the closing verification
	// gate. By default a finish is NOT ground truth that the task is done: when the
	// caller configured VerifyCmd/VerifyLastRun, an explicit finish routes through
	// the SAME verifyTermination gate as prose termination (a finish while the build
	// is red is Unverified, not a false Answered — the harness review's FinishTool
	// hole). Set this true when the finish IS the deliverable and there is nothing to
	// re-verify — a conversational caller whose whole turn is "send a message"
	// (duet's `say`) that sets no VerifyCmd is unaffected either way, since
	// verifyTermination is a no-op without a configured check. Default false.
	FinishToolTrustsCaller bool
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
// seedMessages builds the loop's initial conversation. With no History it is the
// historical single-shot start ("TASK: " + Task), plus env — the ENVIRONMENT
// preamble from observeEnvironment ("" = none) — so the model opens already
// grounded in its cwd and the root listing. With History (a continuing
// conversation, see Session) it clones the prior messages — so the caller's slice
// is never mutated as the loop appends — and adds this turn's input as a plain
// user message (no "TASK:" reframing and NO env preamble, since the model
// already has the context). Both loops call this so the continuation seam
// behaves identically across them.
func seedMessages(cfg Config, env string) []llm.Message {
	if len(cfg.History) == 0 {
		if len(cfg.TaskImages) > 0 {
			return []llm.Message{llm.UserParts(append([]llm.ContentPart{llm.Text("TASK: " + cfg.Task + env)}, imagesAsParts(cfg.TaskImages)...)...)}
		}
		return []llm.Message{llm.User("TASK: " + cfg.Task + env)}
	}
	msgs := make([]llm.Message, 0, len(cfg.History)+1)
	msgs = append(msgs, cfg.History...)
	if len(cfg.TaskImages) > 0 {
		return append(msgs, llm.UserParts(append([]llm.ContentPart{llm.Text(cfg.Task)}, imagesAsParts(cfg.TaskImages)...)...))
	}
	return append(msgs, llm.User(cfg.Task))
}

func imagesAsParts(images []llm.ImagePart) []llm.ContentPart {
	parts := make([]llm.ContentPart, len(images))
	for i, img := range images {
		parts[i] = img
	}
	return parts
}

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
	spiralWindow := cfg.NavSpiralWindow
	if spiralWindow <= 0 {
		spiralWindow = noProgressWindow
	}
	if cfg.Tools == nil {
		cfg.Tools = DefaultTools(cfg.Sandbox, runTimeout)
	}
	// (REVIEW-GATE slice 0) The test fence wraps the mutation tools BEFORE the
	// system prompt is built from them, and the closing gates snapshot their
	// run-start state (fence hashes + the review diff base). Empty fence + nil
	// Reviewer ⇒ both are inert and behavior is byte-identical.
	// Diff-scope wraps FIRST, test-fence LAST, so the fence wins for fenced
	// in-scope paths (the refusal names whichever mechanism refused).
	cfg.Tools = applyDiffScope(cfg.Tools, cfg.DiffScope, cfg.Sandbox)
	cfg.Tools = applyTestFence(cfg.Tools, cfg.TestFence, cfg.Sandbox)
	gs := newGates(ctx, cfg, runTimeout)

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
	messages := seedMessages(seedCfg, observeEnvironment(ctx, cfg.Sandbox)+gs.baselinePreamble())
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

	for i := 1; i <= maxIter; i++ { // (P5) the hard cap lives in the loop header.
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
			// A deadline hit mid-Generate is the wall-clock budget, not a transport fault.
			if cfg.MaxWallClock > 0 && loopCtx.Err() == context.DeadlineExceeded {
				res.Outcome = HitDeadline
				res.Reason = fmt.Sprintf("hit wall-clock budget (%s) mid-turn", cfg.MaxWallClock)
				return gs.upgradeIfVerified(ctx, res), nil
			}
			// (HP-1) The window overflowed and eviction couldn't compact it any
			// further (only TASK + the most recent turn remain). Degrade gracefully
			// to a typed stop rather than surfacing it as a transport fault.
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
		noteUsage(cfg.Obs, i, res.Usage, cfg.MaxTotalTokens)

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
			// (P5/HP-5) Don't trust the done-signal blindly: re-verify the claimed
			// terminal state before accepting it (fence first, then scope, then VerifyCmd).
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
				// Continue-on-fail: a premature finish becomes more work, not a stop.
				// Feed the real failing state back (P4) and keep going.
				cfg.Obs.Note("finish rejected (not verified) — continuing")
				messages = append(messages, llm.User("OBSERVATION:\nNot finished — you answered, but the task is not verified:\n"+reason+"\nKeep working: fix the code and re-run until it passes."))
				continue
			}
			if reason != "" {
				// No budget left (or not in continue-mode): an honest non-pass, not a stored fact.
				res.Outcome = outcome
				res.Answer = arg
				res.Reason = reason
				cfg.Obs.Note("answer not verified — " + reason)
				return res, nil
			}
			// (Empty-answer guard) An `answer` with no content is the model emitting the
			// done-signal without actually answering. Even when verification passed, an
			// empty final answer is not a clean pass — flag it so an empty string can't be
			// recorded as Answered/exit-0.
			if strings.TrimSpace(arg) == "" {
				res.Outcome = Unverified
				res.Reason = "empty final answer — the model stopped without producing an answer"
				cfg.Obs.Note("empty final answer — recording as unverified, not a clean pass")
				return res, nil
			}
			// (REVIEW-GATE slice 1) Stage 2, only now that fence + VerifyCmd are
			// green (execution-first): blocking findings with repair budget left
			// become an observation and more work (the VerifyContinue pattern);
			// with the rounds exhausted they are an honest non-pass.
			if fb, blockReason := gs.reviewFinish(ctx, i < maxIter); fb != "" {
				cfg.Obs.Note("finish rejected (review blockers) — continuing")
				messages = append(messages, llm.User("OBSERVATION:\n"+fb))
				continue
			} else if blockReason != "" {
				res.Outcome = Unverified
				res.Answer = arg
				res.Reason = blockReason
				cfg.Obs.Note("answer blocked by review — " + blockReason)
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
				res.memDone = rememberAsync(ctx, cfg.Obs, cfg.Memory, scope, cfg.Task, arg)
			} else if cfg.Memory != nil {
				cfg.Obs.Note("memory: answer not tool-verified this run — not stored (avoids amplifying guessed/recalled facts)")
			}
			return res, nil
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
		observation := dispatch(loopCtx, cfg.Tools, verb, arg)
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

		// Record the step with the FINAL observation — after every augmentation —
		// so RunResult.Steps and the persisted transcript carry exactly the text
		// the model read, not a pre-nudge draft (review #5).
		step.Observation = observation
		res.Steps = append(res.Steps, step)

		// OBSERVE: the result — including any error — is appended as the next thing
		// the model sees (P2, P4). It is REAL external state, our anchor.
		cfg.Obs.Observation(observation)
		messages = append(messages, llm.User("OBSERVATION:\n"+observation))
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

// diagnoseSource runs cfg.DiagnoseCmd as slice 1's diagnostics SOURCE (a fast
// compile/type check — go build/go vet via the sandbox) and reports its output and
// a three-way diagState. The source/surfacing split (see docs/specs/CODE-INTELLIGENCE.md)
// keeps this single call as the seam a future persistent-gopls client slots
// behind, leaving the loops' surfacing logic untouched. An infra failure to even
// start the check (or a "" command) is diagUnknown, NOT clean: we never fabricate
// a build error out of an infrastructure fault (P6) — and just as importantly we
// never fabricate a GREEN out of one, because the callers reset the stuck counter
// on clean and a transient exec fault on a genuinely-red build would silently
// disarm the feed.
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

// diagState is diagnoseSource's verdict: green, red, or no-signal. The zero
// value is diagUnknown on purpose — "we couldn't check" is the safe default.
type diagState int

const (
	diagUnknown diagState = iota // the check couldn't run — no signal either way.
	diagClean                    // the check ran and exited 0 — genuinely green.
	diagDirty                    // the check ran and failed — report carries the errors.
)

func diagnoseSource(ctx context.Context, cfg Config, timeout time.Duration) (report string, state diagState) {
	if cfg.DiagnoseCmd == "" {
		return "", diagUnknown
	}
	out, err := runOp(ctx, cfg.verifySandbox(), cfg.DiagnoseCmd, timeout)
	if err != nil {
		return "", diagUnknown
	}
	if isRunFailure(out) {
		return out, diagDirty
	}
	return "", diagClean
}

// diagnosticsMessage renders the stuck-with-a-broken-build feed: the failing command
// and its (already clipped by formatRun) output, framed explicitly as INFORMATION, not
// a stop. Shared by both loops so the wording is identical regardless of protocol.
func diagnosticsMessage(cmd, report string) string {
	return fmt.Sprintf("[diagnostics] your recent edits do not build yet — `%s` reports:\n%s\n"+
		"Fix these errors and continue. This is informational, not a stop.", cmd, report)
}

// verifyTimeout resolves the bound for closing VerifyCmd executions. When
// Config.VerifyTimeout is explicitly set it wins; otherwise the bound is the
// LARGER of the resolved per-`run`-tool RunTimeout and 5 minutes — a verify
// suite is routinely slower than a single interactive command, and a too-short
// bound turns "couldn't finish checking" into a false "did not pass".
func verifyTimeout(cfg Config, runTimeout time.Duration) time.Duration {
	if cfg.VerifyTimeout > 0 {
		return cfg.VerifyTimeout
	}
	const floor = 5 * time.Minute
	if runTimeout > floor {
		return runTimeout
	}
	return floor
}

// verifyRun executes cfg.VerifyCmd under the ONE context policy every closing
// verification gate shares — answer, FinishTool, kill, cap, and deadline paths
// alike (review #3; the paths used to diverge, so a cancel at the instant the
// model answered recorded Unverified while the same run killed a turn later got
// upgraded to Answered):
//
//   - A USER cancel skips the check entirely (skipped=true) — the user asked us
//     to stop, so we spend nothing more; the caller reports the unverified /
//     un-upgraded outcome honestly.
//   - Anything else (including a wall-clock or caller deadline expiry) runs the
//     check DETACHED from cancellation (context.WithoutCancel) — an expired
//     budget must not block the final ground-truth check — bounded by the
//     sandbox's own command timeout so it can't hang.
func verifyRun(ctx context.Context, cfg Config, runTimeout time.Duration) (out string, skipped bool, err error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", true, nil
	}
	out, err = runOp(context.WithoutCancel(ctx), cfg.verifySandbox(), cfg.VerifyCmd, verifyTimeout(cfg, runTimeout))
	return out, false, err
}

// upgradeIfVerified flips a non-Answered terminal outcome (a kill or the iteration
// cap) to Answered when VerifyCmd actually passes (P5/HP-5). The verify command is
// the source of truth for "is the task done", so a run whose code is already correct
// must not be reported as a failure just because the loop bailed — the live runs
// showed gpt-5-nano flail on a malformed `bash -lc go test` side-command (tripping
// the stagnant detector) while its real calc.go passed. This is the dual of
// verify-continue: that downgrades a false success, this upgrades a false failure.
// No-op without VerifyCmd, on a user cancel (verifyRun's skip), or when the
// command still fails (a genuine non-pass).
func upgradeIfVerified(ctx context.Context, cfg Config, res *RunResult, runTimeout time.Duration) *RunResult {
	if cfg.VerifyCmd == "" {
		return res
	}
	out, skipped, err := verifyRun(ctx, cfg, runTimeout)
	if !skipped {
		notifyVerify(cfg.Obs, cfg.VerifyCmd, err == nil && !isRunFailure(out))
	}
	if skipped || err != nil || isRunFailure(out) {
		return res
	}
	res.Reason = fmt.Sprintf("completed despite %s — %q passed", res.Outcome, cfg.VerifyCmd)
	res.Outcome = Answered
	if res.Answer == "" {
		res.Answer = fmt.Sprintf("task verified complete (%q passed)", cfg.VerifyCmd)
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
func verifyTermination(ctx context.Context, cfg Config, lastRunFailed bool, runTimeout time.Duration) (reason, verifyOut string) {
	if cfg.VerifyCmd != "" {
		out, skipped, err := verifyRun(ctx, cfg, runTimeout)
		if skipped { // user cancel — no check ran, so the claim stays unconfirmed (and no VerifyResult: nothing was measured).
			return fmt.Sprintf("run canceled before verification command %q could confirm success", cfg.VerifyCmd), ""
		}
		notifyVerify(cfg.Obs, cfg.VerifyCmd, err == nil && !isRunFailure(out))
		if err != nil { // couldn't even start it — we cannot confirm success.
			return fmt.Sprintf("could not run verification command %q: %v", cfg.VerifyCmd, err), ""
		}
		if isRunFailure(out) {
			if isRunTimeout(out) {
				bound := verifyTimeout(cfg, runTimeout)
				return fmt.Sprintf("verification command %q was INCONCLUSIVE (timed out after %s — raise -verify-timeout):\n%s", cfg.VerifyCmd, bound, out), out
			}
			return fmt.Sprintf("verification command %q did not pass:\n%s", cfg.VerifyCmd, out), out
		}
		return "", ""
	}
	if cfg.VerifyLastRun && lastRunFailed {
		return "the most recent command run was still failing and nothing succeeded after it — the task does not look complete", ""
	}
	return "", ""
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

// addUsage sums two Usage values field-by-field so RunResult.Usage accumulates
// the whole run's token cost.
func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		CachedTokens:     a.CachedTokens + b.CachedTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
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
