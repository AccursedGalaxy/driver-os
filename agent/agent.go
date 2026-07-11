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
	"math"
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
	// DefaultMaxTokens is the fallback output cap used when Config.MaxTokens is
	// unset (see agent/loop_shared.go). 1024 was anomalously low and silently
	// truncated long final answers or write_file/edit_file content blocks mid-block,
	// forcing a recovery round-trip. 8192 aligns with provider/anthropic.DefaultMaxTokens.
	DefaultMaxTokens = 8192

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

	autoVerifyBaselineTimeout = 90 * time.Second
	autoVerifyMaxFeedback     = 2

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
	readLineCap  = 150     // (P1, CONTEXT) read_file's unbounded and open-ended reads return at most this many lines.
	// readLineCapExplicit is the larger context tier for an explicitly closed
	// line range, so a model can request a useful bounded slice without paging.
	readLineCapExplicit = 600
	listEntryCap        = 200  // (P1) list_dir caps entries so a huge directory can't flood the window.
	runStreamCap        = 4000 // (P1) run clips each of stdout/stderr to this many runes, head+tail.

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
	KilledSpiral    Outcome = "killed_spiral"     // frontier-aware explore-spiral: a discovery-only cycle (revisiting seen list_dir/search targets) or endless novel wandering (spiralState).
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
	StartedAt   time.Time
	EndedAt     time.Time
	Task        string
	Root        string  // the dir the sandbox was rooted at (Config.Root).
	Outcome     Outcome // how the run ended.
	RescuedFrom Outcome // pre-upgrade Outcome when upgradeIfVerified rescued this run to Answered. Outcome == Answered && RescuedFrom != "" means the model never itself claimed completion — the harness gates proved the work complete after a cap/kill exit.
	Answer      string  // the final answer, set iff Outcome == Answered.
	Reason      string  // human explanation for a non-Answered outcome (kept so the CLI prints the old message verbatim).
	Steps       []Step  // the full trace.
	Iterations  int     // turns taken.
	Usage       llm.Usage
	Err         error // set iff Outcome == ProviderErr.
	// Review is the review-gate record — rounds, every finding with its fate,
	// and the reviewer's token cost (the calibration telemetry). nil when no
	// Reviewer was configured; populated on every loop exit when one was, even
	// if the gate never fired (Skipped says why).
	Review *ReviewReport

	// Guarantees is the typed, machine-readable evidence report for this run.
	Guarantees Guarantees `json:"guarantees"`
	// ConfigRecord is the transcript reproducibility snapshot for this run. It is
	// populated by the native loop after prompt and tool schemas are finalized.
	ConfigRecord      *ConfigRecord
	TerminationPolicy TerminationPolicy `json:"termination_policy"`
	DetectorCounters  DetectorCounters  `json:"detector_counters"`
	// Repro is the repro-first gate report. nil unless ReproFirst was enabled.
	Repro *ReproReport
	// Plan is the plan-stage record — the plan the solver was handed (or why
	// there was none) and the planner's own token cost, kept OUT of Usage so
	// per-role spend stays attributable. nil when no Planner was configured.
	Plan *PlanReport

	SolverCost   *float64 `json:"solver_cost_usd,omitempty"`
	ReviewerCost *float64 `json:"reviewer_cost_usd,omitempty"`
	PlannerCost  *float64 `json:"planner_cost_usd,omitempty"`
	SelectorCost *float64 `json:"selector_cost_usd,omitempty"`
	TotalCost    *float64 `json:"total_cost_usd,omitempty"`
	CostSource   *string  `json:"cost_source,omitempty"`
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
	// ClosingVerification records the authoritative closing gate run that accepted
	// the final state. It is nil when no closing verification succeeded.
	ClosingVerification *VerificationRecord `json:"closing_verification,omitempty"`
	// VerifyInfra is true when a closing VerifyCmd failed because the harness
	// recognized an environment fault rather than a code failure.
	VerifyInfra bool `json:"verify_infra,omitempty"`
	// VerifyInfraSignature is the matched environment-fault signature.
	VerifyInfraSignature string `json:"verify_infra_signature,omitempty"`

	// autoVerify* fields are the Session hand-off for approach (B): Run/RunNative
	// still resolve lazily inside the loop, then Session persists the resolved base
	// config so turns 2..N copy an already-armed (or already-decided-off) gate.
	autoVerifyResolved           bool
	autoVerifyCmd                string
	autoVerifySoft               bool
	autoVerifyProvenance         string
	autoVerifyVerifyContinue     bool
	autoVerifySkipVerifyBaseline bool

	// CacheSumExpectedCached is the run-level sum of expected cached tokens per
	// the prefix-cache model (Measurement 3 in eval/scripts/read_dup_pass.py):
	// turn 1 expected=0; turn i>1 expected=min(P_{i-1}, P_i).
	CacheSumExpectedCached int `json:"sum_expected_cached,omitempty"`
	// CacheSumCached is the run-level sum of actual cached tokens across turns.
	CacheSumCached int `json:"sum_cached,omitempty"`
	// CacheMiss is the run-level sum of cache misses (expected − actual, clamped >= 0).
	CacheMiss int `json:"cache_miss,omitempty"`
	// CacheHitPct is the run-level cache-hit percentage: 100 * sum_cached / sum_expected_cached,
	// or 0 when sum_expected_cached is 0.
	CacheHitPct float64 `json:"cache_hit_pct,omitempty"`

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

// ReviewPolicy selects how an armed review gate handles infrastructure failures
// and unavailable workspace baselines. The zero value preserves the historical
// library default: a configured Reviewer is required and its baseline must be
// available.
type ReviewPolicy int

const (
	// ReviewPolicyDefault requires an armed reviewer to succeed and requires its
	// workspace baseline. With no Reviewer, the review gate is disabled.
	ReviewPolicyDefault ReviewPolicy = iota
	// ReviewPolicyRequired explicitly requires review infrastructure to succeed.
	ReviewPolicyRequired
	// ReviewPolicyFailOpen records review infrastructure failures without
	// downgrading an otherwise verified run.
	ReviewPolicyFailOpen
	// ReviewPolicyOptional permits an unavailable review baseline, while review
	// infrastructure failures still block when the reviewer can be armed.
	ReviewPolicyOptional
	// ReviewPolicyRequiredOptional explicitly requires review infrastructure but
	// permits an unavailable review baseline.
	ReviewPolicyRequiredOptional
	// ReviewPolicyFailOpenOptional permits both an unavailable baseline and
	// fail-open handling of review infrastructure failures.
	ReviewPolicyFailOpenOptional
)

// ReviewPolicyFrom converts the legacy flag representation into a policy.
// Required and failOpen are contradictory; all other combinations are the
// configurations historically constructed by this repository.
func ReviewPolicyFrom(required, failOpen, optional bool) (ReviewPolicy, error) {
	if required && failOpen {
		return ReviewPolicyDefault, fmt.Errorf("review required and fail-open cannot both be enabled")
	}
	switch {
	case required && optional:
		return ReviewPolicyRequiredOptional, nil
	case failOpen && optional:
		return ReviewPolicyFailOpenOptional, nil
	case required:
		return ReviewPolicyRequired, nil
	case failOpen:
		return ReviewPolicyFailOpen, nil
	case optional:
		return ReviewPolicyOptional, nil
	default:
		return ReviewPolicyDefault, nil
	}
}

func (p ReviewPolicy) valid() bool {
	return p >= ReviewPolicyDefault && p <= ReviewPolicyFailOpenOptional
}
func (p ReviewPolicy) required() bool {
	return p == ReviewPolicyRequired || p == ReviewPolicyRequiredOptional
}
func (p ReviewPolicy) failOpen() bool {
	return p == ReviewPolicyFailOpen || p == ReviewPolicyFailOpenOptional
}
func (p ReviewPolicy) optional() bool {
	return p == ReviewPolicyOptional || p == ReviewPolicyRequiredOptional || p == ReviewPolicyFailOpenOptional
}

// FailOpen reports whether review infrastructure failures are advisory.
func (p ReviewPolicy) FailOpen() bool { return p.failOpen() }

// Optional reports whether an unavailable review baseline may be skipped.
func (p ReviewPolicy) Optional() bool { return p.optional() }

// Config is everything Run needs. Model, Sandbox, and Task are required; the
// rest default sensibly (nil Tools -> DefaultTools, nil Memory -> no cross-run
// recall, nil Obs -> silent).
type Config struct {

	// verifyBaselineCache is Session-owned state, injected by SendParts. It is
	// deliberately unexported so one-shot runs always start cold.
	verifyBaselineCache *verifyBaselineCache

	// BinaryLabel is the legacy v1-v3 conflated binary label retained for
	// transcript compatibility. New callers should set BinaryIdentity instead.
	BinaryLabel string

	// BinaryIdentity identifies the built artifact for transcript config records.
	// It is recording-only and never affects behavior.
	BinaryIdentity string

	// InvocationSurface identifies how the artifact was invoked (for example,
	// driver-run). It is recording-only and never affects behavior.
	InvocationSurface string

	// RequestedProtocol and ProtocolFallbackReason are CLI provenance supplied by
	// the caller. The effective protocol is selected by Run or RunNative.
	RequestedProtocol      string
	ProtocolFallbackReason string

	// TrustProfile identifies the selected trust profile for transcript config
	// records. It is recording-only and never affects behavior.
	TrustProfile string

	// ExecProfileName identifies the selected execution profile for transcript
	// config records. It is recording-only and never affects behavior, mirroring
	// TrustProfile.
	ExecProfileName string

	// ExecProfileHash identifies the selected execution profile content for
	// transcript config records. It is recording-only and never affects behavior,
	// mirroring TrustProfile.
	ExecProfileHash string

	// CLIOverrides records the CLI override provenance for transcript config
	// records. It is recording-only and never affects behavior, mirroring
	// TrustProfile.
	CLIOverrides []string

	// Resolution provenance is recording-only and never affects behavior.
	RequiredTrust   string
	Canonical       bool
	FieldProvenance map[string]string

	// ApprovalPolicyName identifies the selected approval policy for transcript
	// config records. It is recording-only and never affects behavior.
	ApprovalPolicyName string

	// ApprovalPolicyHash identifies the selected approval policy content for
	// transcript config records. It is recording-only and never affects behavior.
	ApprovalPolicyHash string

	// evidence is run-local observation state threaded through gates and helpers.
	evidence *evidenceLog
	Model    llm.Provider    // required: the (context) -> text engine.
	Sandbox  sandbox.Sandbox // required: the isolation boundary every effect flows through (P2).
	Memory   mneme.Memory    // optional: cross-run long-term memory; nil = stateless.

	// DisableMemoryStore suppresses the post-answer mneme.Add call while leaving
	// recall enabled. Ladder attempts use this so losing attempts can benefit from
	// prior memory without leaking their own failed observations into future runs.
	DisableMemoryStore bool

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
	Root    string // optional: the dir Sandbox is rooted at; recorded in RunResult.Root.

	BootContext bool // include the boot-context Go dependency digest in the opening ENVIRONMENT preamble; set by the -boot-context CLI flag (default true)

	// StandingContext opts into a never-persisted per-turn trailer summarizing the
	// model's own diff since run start plus the last run/verify status. Off at the
	// library level (zero value) so callers that don't opt in pay no extra git or
	// prompt cost; the interactive TUI (cmd/driver) opts in by default, headless
	// cmd/agent leaves it off.
	StandingContext bool

	Obs Observer // optional: live progress sink; nil = silent.

	// PerfMark receives optional latency instrumentation from the native tool
	// loop. It is nil for callers that do not collect performance timelines.
	PerfMark func(event string, attrs map[string]any)

	// ModelInfo describes the selected solver model's known limits. A zero value
	// means unknown and disables proactive context-window telemetry.
	ModelInfo llm.ModelInfo

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

	// RequireNetworkOff is a SAFETY PRECONDITION: before the first model call,
	// Run/RunNative refuse when Sandbox.Capabilities reports network access. It
	// fails closed for a nil Sandbox. The zero value preserves trusted callers'
	// existing behavior.
	RequireNetworkOff bool

	// All three default sensibly when zero (P5/P7 — termination knobs are OURS):
	MaxIterations int           // 0 = DefaultMaxIterations. The hard cap on think->act->observe turns.
	MaxTokens     int           // 0 = DefaultMaxTokens. Per-turn output cap on the model call.
	RunTimeout    time.Duration // 0 = defaultRunTimeout. Wall-clock kill for a single `run` command.

	// TerminationPolicy groups no-progress detector thresholds. Its zero value resolves to DefaultTerminationPolicy.
	TerminationPolicy TerminationPolicy

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

	// CodeAct, when true, appends a "code-as-action" instruction block to the
	// native-loop system prompt (see resolveSystemPrompt): it steers the model to
	// treat `run` shell as its primary action — composing/executing code to make
	// and verify changes — rather than issuing many discrete file-tool calls.
	// An A/B experiment knob (docs/specs/CODEACT-SCREEN.md); arms differ in it alone.
	CodeAct bool

	// ReproFirst enforces a red-at-base/green-after-fix reproduction test in the
	// native tool protocol. The solver must write a new test file and call
	// declare_repro before answering, unless it explicitly skip_repro's with a
	// reason.
	ReproFirst bool

	// ReproGate, when true, appends a reproduction-first disposition to the solver system prompt.
	ReproGate bool

	// BatchReads, when true, appends a "batch independent reads" instruction
	// block to the native-loop system prompt (see resolveSystemPrompt): it
	// steers the model to emit several parallel-safe read-only tool calls in
	// ONE turn instead of one per turn, so the harness's parallel prefetch
	// (prefetchLeadingReadOnly) can fetch them concurrently. An A/B arm knob.
	BatchReads bool

	// ReadWindow overrides read_file's line window (max lines a range-less or
	// over-long read returns before clipping). 0 = the built-in default (150).
	// Only consulted when Config.Tools is nil and the default toolbox is built.
	ReadWindow int
	// ReadOutline, when true, appends a compact file structure map (symbols +
	// line numbers) to a CLIPPED read_file so the model can jump to the right
	// range. Only consulted when Config.Tools is nil (default toolbox path).
	ReadOutline bool

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

	// MaxTotalCostUSD bounds the run's CUMULATIVE dollar spend, priced by CostFn
	// from the run's cumulative Usage. It mirrors MaxTotalTokens but in dollars:
	// checked at the turn boundary AFTER a paid generation, so the crossing turn
	// is kept and overshoot is bounded by one turn's spend; the loop then ends as
	// HitBudget and closing verification may still upgrade it. 0 = off.
	MaxTotalCostUSD float64

	// AllowUnpricedSpend permits a configured dollar budget to continue when spend
	// cannot be priced. The default false fails closed as HitBudget; set true only
	// when an advisory budget is acceptable.
	AllowUnpricedSpend bool

	// CostFn prices this run's solver model from cumulative Usage. The agent does
	// not know catalog model ids; callers close over the model id in this function.
	// When MaxTotalCostUSD is set but CostFn is nil or returns ok=false, the loop
	// fails closed unless AllowUnpricedSpend explicitly opts into note-once-and-
	// continue behavior.
	CostFn func(llm.Usage) (float64, bool)

	// SolverModel is the solver model's id, so the loop can price solver turns into
	// Spend (the agent otherwise does not know catalog model ids). Empty disables
	// solver pricing in Spend.
	SolverModel string

	// Spend is the run's shared role-aware dollar accumulator (solver + reviewer +
	// planner, each priced at its own model). When non-nil it is the source of truth
	// for MaxTotalCostUSD, superseding CostFn. Shared by POINTER so it accumulates
	// across repair rounds and gate/plan calls. nil = fall back to CostFn (solver-only).
	Spend *Spend

	// VerifyCmd is the closing VERIFICATION gate (P5/HP-5): a success command the
	// caller names (e.g. "go test ./...") that is re-run when the model finishes.
	// A non-zero exit downgrades the terminal Answered to Unverified — turning a
	// model that stopped while the task was still broken (DOGFOOD R9/R10's
	// termination-by-silence false positives: narrated intent, acknowledged
	// failure, hallucinated success) into an honest non-pass instead of exit-0
	// success. Empty = no closing check. The harness does NOT guess the success
	// criterion; the caller states it.
	VerifyCmd string

	// AutoVerify opts into deriving a conservative VerifyCmd from root project
	// markers when VerifyCmd is empty. It is off by default in agent core; the
	// daily driver enables it after prefs/flags resolve. Eval never arms it.
	AutoVerify bool

	// AutoVerifySoft marks a harness-chosen VerifyCmd whose disposition must never
	// be worse than the empty-gate baseline. autoVerifyProvenance names the marker
	// that produced it for notes and the boot preamble. autoVerifyResolved is a
	// session guard: once an interactive Session has attempted auto-verify
	// resolution, later turns must not re-derive or re-run the preflight on WIP.
	AutoVerifySoft       bool
	autoVerifyProvenance string
	autoVerifyResolved   bool

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

	// ReviewPolicy controls whether review failures block and whether an
	// unavailable baseline may be skipped. Its zero value preserves the default
	// behavior of requiring any configured Reviewer.
	ReviewPolicy ReviewPolicy

	// RequireDiff makes an empty final workspace diff a failed completion rather
	// than an answer. It requires a git baseline at startup and is opt-in because
	// some tasks are legitimate no-ops.
	RequireDiff bool

	// ReviewUnverified runs one advisory, report-only reviewer pass when a run
	// ends Unverified with a non-empty diff before the normal review gate would
	// fire. The verdict enriches salvage telemetry but never changes the run
	// outcome or reason. CLI defaults it on; callers may leave it false to opt out.
	ReviewUnverified bool

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

	// NavSpiralWindow overrides the explore-spiral detector's CYCLE window — the
	// number of discovery-only turns REVISITING only already-seen targets
	// (list_dir paths / search queries) that ends the run as KilledSpiral. 0 = the
	// default noProgressWindow. The detector is frontier/state-aware (spiralState):
	// a discovery turn that reveals any NEW target is orientation and never counts
	// toward this window, so novel top-down orientation of a large repo is never
	// false-killed; a hard wandering bound (spiralWanderMultiple × this window)
	// caps endless novel discovery. The bounds are deterministic — no model-family
	// or reasoning variance. It exists
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

	// FinishToolTrustsCaller lets a FinishTool call vouch for task completion while
	// still enforcing safety boundaries. By default a finish is NOT ground truth
	// that the task is done: when the caller configured VerifyCmd/VerifyLastRun, an
	// explicit finish routes through the SAME completion gate as prose termination
	// (a finish while the build is red is Unverified, not a false Answered — the
	// harness review's FinishTool hole). Set this true when the finish IS the
	// deliverable and there is nothing to re-verify — a conversational caller whose
	// whole turn is "send a message". Even then, RequireDiff, the test fence,
	// diff scope, caller-cancel check, empty-answer guard, and grounded-memory
	// policy still run.
	// Default false.
	FinishToolTrustsCaller bool
}

// Validate checks contradictory or unsafe pure configuration before setup does
// any environmental work. Workspace and sandbox capability checks remain in the
// stages that can inspect those resources.
func (c Config) Validate() error {
	if c.Model == nil {
		return setupErr("invalid_config", "Model must not be nil")
	}
	if c.Sandbox == nil && !c.RequireNetworkOff {
		return setupErr("invalid_config", "Sandbox must not be nil")
	}
	if !c.ReviewPolicy.valid() {
		return setupErr("invalid_review_config", "ReviewPolicy is not a recognized value")
	}
	if c.ReviewPolicy.required() && c.Reviewer == nil {
		return setupErr("invalid_review_config", "required review policy needs a Reviewer")
	}
	if c.MaxIterations < 0 || c.MaxTokens < 0 || c.RunTimeout < 0 || c.VerifyTimeout < 0 ||
		c.MaxWallClock < 0 || c.MaxTotalTokens < 0 || c.MaxTotalCostUSD < 0 ||
		c.ReviewRounds < 0 || c.FinishNudgeWindow < 0 || c.NavSpiralWindow < 0 ||
		c.AnswerNudgeWindow < 0 || c.ChurnNudgeRuns < 0 || c.ReadWindow < 0 {
		return setupErr("invalid_config", "budgets, caps, timeouts, rounds, and windows must not be negative")
	}
	return nil
}

// checkSandboxFloor enforces Config.MinIsolation and Config.RequireNetworkOff and
// is the FIRST thing both loops do. It returns a non-nil RefusedUnsafe RunResult
// — to be returned directly, before any model call or verification — when the
// Sandbox is weaker than required. It fails CLOSED: a nil Sandbox under either
// floor is a refusal, not a panic. The caller must return this result AS-IS and
// must NOT route it through upgradeIfVerified — a refused run must never execute
// VerifyCmd on the unsafe sandbox.
func checkSandboxFloor(cfg Config) *RunResult {
	var caps sandbox.Capabilities
	haveCaps := cfg.Sandbox != nil
	if haveCaps {
		caps = cfg.Sandbox.Capabilities()
		if cfg.evidence != nil {
			cfg.evidence.observedIsolation = caps.Isolation.String()
			observedNetwork := caps.Network
			cfg.evidence.observedNetwork = &observedNetwork
		}
	}
	if cfg.MinIsolation > sandbox.IsolationNone {
		have := sandbox.IsolationNone
		if haveCaps {
			have = caps.Isolation
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
	}
	if cfg.RequireNetworkOff && (!haveCaps || caps.Network) {
		return &RunResult{
			Task:    cfg.Task,
			Root:    cfg.Root,
			Outcome: RefusedUnsafe,
			Reason:  "refused: task requires network off but the sandbox network floor was violated",
		}
	}
	return nil
}

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
