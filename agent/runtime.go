package agent

// The S6a Requested/Resolved split (docs/specs/PROFILES.md §7): the loops take
// three values instead of the flat Config —
//
//	spec    runspec.ResolvedSpec — complete, immutable policy DATA, produced
//	        exactly once by runspec.Resolve. The loops read it; they never
//	        repair it (completeness is asserted at entry, never re-derived).
//	rt      Runtime — injected dependencies. Never serialized; identity that
//	        matters to reproducibility (model slugs, sandbox kind) lives in the
//	        spec or RecordMeta.
//	content Content — the per-run payload (task, images, history, root).
//
// There is deliberately NO Config-shaped adapter between Resolve output and
// the loops: an adapter that copies fields is the manual-copy drift class this
// design deletes (§7.5 S6a).

import (
	"time"

	"github.com/AccursedGalaxy/driver-os/internal/runspec"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/mneme"
)

// Runtime is the injected dependency bundle for one run (spec §7.1 "runtime
// bindings"). Model and Sandbox are required; the rest default sensibly (nil
// Tools -> DefaultTools, nil Memory -> no cross-run recall, nil Obs -> silent).
type Runtime struct {
	Model   llm.Provider    // required: the (context) -> text engine.
	Sandbox sandbox.Sandbox // required: the isolation boundary every effect flows through (P2).

	// VerifySandbox is the sandbox the CLOSING gates (VerifyCmd/DiagnoseCmd) run
	// on, when it must differ from Sandbox. nil ⇒ Sandbox. See ../SESSION.md.
	VerifySandbox sandbox.Sandbox

	Memory mneme.Memory    // optional: cross-run long-term memory; nil = stateless.
	Tools  map[string]Tool // optional: nil = DefaultTools(Sandbox).
	Obs    Observer        // optional: live progress sink; nil = silent.

	// PerfMark receives optional latency instrumentation from the native tool
	// loop. nil for callers that do not collect performance timelines.
	PerfMark func(event string, attrs map[string]any)

	// ModelInfo describes the selected solver model's known limits. A zero value
	// means unknown and disables proactive context-window telemetry.
	ModelInfo llm.ModelInfo

	// CostFn prices this run's solver model from cumulative Usage; Spend is the
	// shared role-aware dollar accumulator that supersedes it when non-nil.
	CostFn func(llm.Usage) (float64, bool)
	Spend  *Spend

	// Reviewer arms the review gate; Planner arms the opening plan stage.
	// Implementations live outside agent (council) — injected to avoid the cycle.
	Reviewer Reviewer
	Planner  Planner

	// Record is the recording-only invocation identity and resolution provenance
	// for the transcript ConfigRecord. It never affects behavior. (S6c replaces
	// this caller-supplied block with trace-derived serialization.)
	Record RecordMeta

	// Session-owned state, injected by Session (unexported so one-shot runs
	// always start cold).
	verifyBaselineCache *verifyBaselineCache
	sessionVerify       *verifyState // a Session's persisted verification state; nil = one-shot.
}

// verifySandbox returns the sandbox the closing verification/diagnostics
// commands run on: VerifySandbox when set, else Sandbox.
func (rt Runtime) verifySandbox() sandbox.Sandbox {
	if rt.VerifySandbox != nil {
		return rt.VerifySandbox
	}
	return rt.Sandbox
}

// obs returns the observer, never nil.
func (rt Runtime) obs() Observer {
	if rt.Obs == nil {
		return nopObserver{}
	}
	return rt.Obs
}

// Content is the per-run payload (spec §7.1): recorded separately from policy
// (the task hash already rides in bundle identity), never part of ConfigSHA256.
type Content struct {
	Task       string          // required: the goal (this turn's user input when continuing).
	TaskImages []llm.ImagePart // optional image parts for THIS turn's user message.
	// History is a prior conversation to CONTINUE from (the continuation seam,
	// see Session). The loop clones it before appending.
	History []llm.Message
	Root    string // optional: the dir Sandbox is rooted at; recorded in RunResult.Root.
}

// RecordMeta is the recording-only invocation identity and resolution
// provenance embedded in ConfigRecord. Every field mirrors a former Config
// field documented as "recording-only and never affects behavior".
type RecordMeta struct {
	// BinaryLabel is the legacy v1-v3 conflated binary label retained for
	// transcript compatibility. New callers set BinaryIdentity instead.
	BinaryLabel            string
	BinaryIdentity         string
	InvocationSurface      string
	RequestedProtocol      string
	ProtocolFallbackReason string
	TrustProfile           string
	ExecProfileName        string
	ExecProfileHash        string
	CLIOverrides           []string
	RequiredTrust          string
	Canonical              bool
	FieldProvenance        map[string]string
	ApprovalPolicyName     string
	ApprovalPolicyHash     string
}

// verifyState is the run-local MUTABLE verification state, seeded from the
// spec's verification domain. Runtime derivation (resolveAutoVerify) mutates
// THIS, never the spec — the held ResolvedSpec keeps the pre-run values
// (PROFILES.md §7.3; the S6c runtime-resolution record will serialize the
// derivation events). A Session persists one across turns.
type verifyState struct {
	Cmd                string
	Timeout            time.Duration
	AutoVerify         bool
	AutoVerifySoft     bool
	SkipVerifyBaseline bool
	AbortOnRedBaseline bool
	VerifyLastRun      bool
	VerifyContinue     bool

	provenance string // the project marker that produced an auto-derived Cmd.
	resolved   bool   // the one-per-session auto-verify decision has been made.

	// events is the append-only runtime-resolution sequence (PROFILES.md §7.3):
	// values derived OUTSIDE Resolve that control behavior (an auto-derived
	// VerifyCmd, a session /verify override) are not config and never mutate
	// the spec, but they execute code — so each derivation is recorded as an
	// ordered event, serialized into the ConfigRecord and hashed into bundle
	// identity. A singular "selected value" slot would let overwrites hide
	// temporal divergence; the sequence cannot.
	events []RuntimeResolution
}

// RuntimeResolution is one runtime-derivation event (§7.3): the derived value,
// where it came from, and the gate that consumes it.
type RuntimeResolution struct {
	Seq        int    `json:"seq"`
	Phase      string `json:"phase"` // "pre-run" | "session"
	Field      string `json:"field"` // "verify_cmd"
	Value      string `json:"value"`
	Provenance string `json:"provenance"` // project marker (go.mod, …) or "user"
	Consumer   string `json:"consumer"`   // "verify-gate"
	Disarmed   bool   `json:"disarmed,omitempty"`
	Note       string `json:"note,omitempty"`
}

func (vs *verifyState) recordResolution(phase, field, value, provenance, consumer string, disarmed bool, note string) {
	vs.events = append(vs.events, RuntimeResolution{
		Seq: len(vs.events) + 1, Phase: phase, Field: field, Value: value,
		Provenance: provenance, Consumer: consumer, Disarmed: disarmed, Note: note,
	})
}

func newVerifyState(pol runspec.PolicyValue) *verifyState {
	return &verifyState{
		Cmd:                pol.VerifyCmd,
		Timeout:            pol.VerifyTimeout,
		AutoVerify:         pol.AutoVerify,
		AutoVerifySoft:     pol.AutoVerifySoft,
		SkipVerifyBaseline: pol.SkipVerifyBaseline,
		AbortOnRedBaseline: pol.AbortOnRedBaseline,
		VerifyLastRun:      pol.VerifyLastRun,
		VerifyContinue:     pol.VerifyContinue,
	}
}

// runVerifyState returns the verification state for one loop invocation: the
// Session-persisted state (copied, so the loop's mutations flow back only via
// the explicit RunResult hand-off) when present, else a fresh seed from the
// spec.
func runVerifyState(pol runspec.PolicyValue, rt Runtime) *verifyState {
	if rt.sessionVerify != nil {
		vs := *rt.sessionVerify
		return &vs
	}
	return newVerifyState(pol)
}

// validateRun checks the cross-cutting policy×runtime contradictions that
// resolution alone cannot see (Resolve validates pure policy; this validates
// the bindings against it). It is the loop-entry twin of the old
// Config.Validate, minus the pure-policy range checks Resolve now owns.
func validateRun(pol runspec.PolicyValue, rt Runtime) error {
	if rt.Model == nil {
		return setupErr("invalid_config", "Model must not be nil")
	}
	if rt.Sandbox == nil && !pol.RequireNetworkOff {
		return setupErr("invalid_config", "Sandbox must not be nil")
	}
	rp := ReviewPolicy(pol.ReviewPolicy)
	if !rp.valid() {
		return setupErr("invalid_review_config", "ReviewPolicy is not a recognized value")
	}
	if rp.required() && rt.Reviewer == nil {
		return setupErr("invalid_review_config", "required review policy needs a Reviewer")
	}
	return nil
}
