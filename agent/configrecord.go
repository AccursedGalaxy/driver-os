package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/internal/runspec"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/memory"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// ConfigRecord schema v9 (PROFILES.md §7.3 / S6c): the lazily re-derived
// EffectiveConfig projection is replaced by the CANONICAL serializations of
// the values the run actually held — the resolved policy value (whose hash is
// ConfigSHA256: two runs with the same effective policy share it regardless of
// how each value was reached), the resolution trace (its own hash, so identity
// and audit questions stay separable), and the append-only runtime-resolution
// event sequence for values derived outside Resolve. v8's Effective survives
// only to decode old transcripts.
const configRecordSchemaVersion = 9

// Binary and invocation-surface identifiers are stable transcript values.
const (
	BinaryIdentityDriver = "driver"

	InvocationSurfaceDriverRun   = "driver-run"
	InvocationSurfaceDriverAgent = "driver-agent"
	InvocationSurfaceDriverTUI   = "driver-tui"
)

// ConfigRecord is the reproducibility record embedded in transcripts. It records
// today's effective config plus stable hashes and the selected profile identities
// and override provenance. Binary is the legacy, conflated binary label from
// schema v1-v3 records; it remains decodable for old transcripts. New records
// use BinaryIdentity and InvocationSurface for the two distinct facts.
type ConfigRecord struct {
	SchemaVersion int `json:"schema_version"`
	// Binary is the legacy v1-v3 conflated invoking-binary label. It is retained
	// solely so old transcript JSON continues to decode.
	Binary            string `json:"binary,omitempty"`
	BinaryIdentity    string `json:"binary_identity,omitempty"`
	InvocationSurface string `json:"invocation_surface,omitempty"`
	// RequestedProtocol and ProtocolFallbackReason describe CLI routing provenance.
	// They intentionally remain outside Effective so they do not affect ConfigSHA256.
	RequestedProtocol      string `json:"requested_protocol,omitempty"`
	ProtocolFallbackReason string `json:"protocol_fallback_reason,omitempty"`
	HarnessCommit          string `json:"harness_commit,omitempty"`
	HarnessDirty           bool   `json:"harness_dirty,omitempty"`
	PromptSHA256           string `json:"prompt_sha256"`
	ToolSchemaSHA256       string `json:"tool_schema_sha256"`
	// ConfigSHA256 is the hash of the canonical POLICY-VALUE serialization
	// (runspec.ResolvedSpec.ConfigSHA256): two runs with the same effective
	// policy share it regardless of HOW each value was reached. It does not
	// prove semantic task equivalence, environment identity, unrecorded runtime
	// state, or provider-side behavior.
	ConfigSHA256 string `json:"config_sha256"`

	// EffectiveProtocol is the wire protocol the loop actually ran ("text" /
	// "tools") — loop-selected, so recorded beside the policy, not inside it.
	EffectiveProtocol string `json:"effective_protocol,omitempty"`

	// Policy is the canonical policy value the loop held (v9).
	Policy *runspec.PolicyValue `json:"policy,omitempty"`
	// ResolutionTrace is the per-field provenance captured at Resolve, under
	// its own hash — "same policy?" and "same way of asking for it?" stay
	// separable questions (§7.3).
	ResolutionTrace       *runspec.Trace `json:"resolution_trace,omitempty"`
	ResolutionTraceSHA256 string         `json:"resolution_trace_sha256,omitempty"`
	// RuntimeResolutions is the append-only runtime-derivation event sequence
	// (auto-verify, session /verify overrides) with its hash — included in
	// bundle identity so two runs sharing ConfigSHA256 cannot execute different
	// derived verify commands invisibly.
	RuntimeResolutions      []RuntimeResolution `json:"runtime_resolutions,omitempty"`
	RuntimeResolutionSHA256 string              `json:"runtime_resolution_sha256,omitempty"`
	// Bindings records which runtime dependencies were configured (presence
	// only — the bindings themselves are never serialized).
	Bindings RuntimeBindings `json:"bindings"`

	// Effective is the legacy v<=8 projection, retained ONLY so old transcript
	// JSON continues to decode; nil in v9 records.
	Effective          *EffectiveConfig  `json:"effective,omitempty"`
	TrustProfile       *string           `json:"trust_profile"`
	ExecProfile        *string           `json:"exec_profile"`
	ExecProfileHash    string            `json:"exec_profile_hash,omitempty"`
	CLIOverrides       []string          `json:"cli_overrides,omitempty"`
	RequiredTrust      string            `json:"required_trust,omitempty"`
	Canonical          bool              `json:"canonical"`
	FieldProvenance    map[string]string `json:"field_provenance,omitempty"`
	ApprovalPolicyName string            `json:"approval_policy_name,omitempty"`
	ApprovalPolicyHash string            `json:"approval_policy_hash,omitempty"`
}

// EffectiveConfig is the serializable projection of Config fields that affect
// agent behavior. Runtime objects and task content are deliberately excluded.
type EffectiveConfig struct {
	// EffectiveProtocol changes the model wire protocol and is therefore hashed.
	EffectiveProtocol       string            `json:"effective_protocol"`
	DisableMemoryStore      bool              `json:"disable_memory_store"`
	Persona                 string            `json:"persona,omitempty"`
	MemoryScope             memory.Scope      `json:"memory_scope"`
	BootContext             bool              `json:"boot_context"`
	StandingContext         bool              `json:"standing_context"`
	Stream                  bool              `json:"stream"`
	MinIsolation            sandbox.Isolation `json:"min_isolation"`
	RequireNetworkOff       bool              `json:"require_network_off"`
	MaxIterations           int               `json:"max_iterations"`
	MaxTokens               int               `json:"max_tokens"`
	RunTimeout              time.Duration     `json:"run_timeout"`
	VerifyTimeout           time.Duration     `json:"verify_timeout"`
	ReasoningEffort         string            `json:"reasoning_effort,omitempty"`
	PromptProfile           string            `json:"prompt_profile,omitempty"`
	CodeAct                 bool              `json:"code_act"`
	ReproFirst              bool              `json:"repro_first"`
	ReproGate               bool              `json:"repro_gate"`
	BatchReads              bool              `json:"batch_reads"`
	ReadWindow              int               `json:"read_window"`
	ReadOutline             bool              `json:"read_outline"`
	MaxWallClock            time.Duration     `json:"max_wall_clock"`
	MaxTotalTokens          int               `json:"max_total_tokens"`
	MaxTotalCostUSD         float64           `json:"max_total_cost_usd"`
	AllowUnpricedSpend      bool              `json:"allow_unpriced_spend"`
	SolverModel             string            `json:"solver_model,omitempty"`
	ModelConfigured         bool              `json:"model_configured"`
	MemoryConfigured        bool              `json:"memory_configured"`
	VerifySandboxConfigured bool              `json:"verify_sandbox_configured"`
	CostFnConfigured        bool              `json:"cost_fn_configured"`
	SpendConfigured         bool              `json:"spend_configured"`
	ModelInfo               llm.ModelInfo     `json:"model_info"`
	ReviewerIdentity        string            `json:"reviewer_identity,omitempty"`
	PlannerIdentity         string            `json:"planner_identity,omitempty"`

	VerifyCmd              string            `json:"verify_cmd,omitempty"`
	AutoVerify             bool              `json:"auto_verify"`
	AutoVerifySoft         bool              `json:"auto_verify_soft"`
	SkipVerifyBaseline     bool              `json:"skip_verify_baseline"`
	AbortOnRedBaseline     bool              `json:"abort_on_red_baseline"`
	VerifyLastRun          bool              `json:"verify_last_run"`
	ChurnNudgeRuns         int               `json:"churn_nudge_runs"`
	VerifyContinue         bool              `json:"verify_continue"`
	TestFence              []string          `json:"test_fence,omitempty"`
	DiffScope              []string          `json:"diff_scope,omitempty"`
	RequireDiff            bool              `json:"require_diff"`
	ReviewConfigured       bool              `json:"review_configured"`
	ReviewPolicy           int               `json:"review_policy"`
	ReviewUnverified       bool              `json:"review_unverified"`
	ReviewRounds           int               `json:"review_rounds"`
	PlannerConfigured      bool              `json:"planner_configured"`
	FinishNudgeWindow      int               `json:"finish_nudge_window"`
	DiagnoseCmd            string            `json:"diagnose_cmd,omitempty"`
	DiagnoseAfterEdits     int               `json:"diagnose_after_edits"`
	NavSpiralWindow        int               `json:"nav_spiral_window"`
	TerminationPolicy      TerminationPolicy `json:"termination_policy"`
	AnswerNudgeWindow      int               `json:"answer_nudge_window"`
	FinishTool             string            `json:"finish_tool,omitempty"`
	FinishToolConfigured   bool              `json:"finish_tool_configured"`
	FinishToolTrustsCaller bool              `json:"finish_tool_trusts_caller"`
}

// RuntimeBindings records the PRESENCE of injected runtime dependencies plus
// their reproducibility-relevant identity (model limits, reviewer/planner
// implementation types). The bindings themselves are never serialized (§7.1).
type RuntimeBindings struct {
	ModelConfigured         bool          `json:"model_configured"`
	MemoryConfigured        bool          `json:"memory_configured"`
	VerifySandboxConfigured bool          `json:"verify_sandbox_configured"`
	CostFnConfigured        bool          `json:"cost_fn_configured"`
	SpendConfigured         bool          `json:"spend_configured"`
	ReviewConfigured        bool          `json:"review_configured"`
	PlannerConfigured       bool          `json:"planner_configured"`
	ModelInfo               llm.ModelInfo `json:"model_info"`
	ReviewerIdentity        string        `json:"reviewer_identity,omitempty"`
	PlannerIdentity         string        `json:"planner_identity,omitempty"`
}

func runtimeBindingsOf(rt Runtime) RuntimeBindings {
	return RuntimeBindings{
		ModelConfigured: rt.Model != nil, MemoryConfigured: rt.Memory != nil,
		VerifySandboxConfigured: rt.VerifySandbox != nil, CostFnConfigured: rt.CostFn != nil,
		SpendConfigured: rt.Spend != nil, ReviewConfigured: rt.Reviewer != nil,
		PlannerConfigured: rt.Planner != nil, ModelInfo: rt.ModelInfo,
		ReviewerIdentity: runtimeImplementationIdentity(rt.Reviewer),
		PlannerIdentity:  runtimeImplementationIdentity(rt.Planner),
	}
}

// newConfigRecord constructs the single shared reproducibility record path
// from the run's ACTUALLY HELD values: the resolved spec (canonical policy +
// trace), the runtime bindings, and the run-local verification state (post
// auto-verify derivation — its events ride the runtime-resolution sequence).
// Callers supply the exact protocol prompt, its stable tool representation,
// and the loop that actually ran. Text representations are textToolGrammar
// values; native representations are []llm.Tool API schemas.
func newConfigRecord(spec runspec.ResolvedSpec, rt Runtime, vs *verifyState, systemPrompt string, toolRepresentation any, protocols ...string) *ConfigRecord {
	effectiveProtocol := "tools" // compatibility default for direct legacy callers.
	if len(protocols) > 0 {
		effectiveProtocol = protocols[0]
	}
	pol := spec.Policy()
	trace := spec.Trace()
	meta := rt.Record
	binaryIdentity := meta.BinaryIdentity
	if binaryIdentity == "" {
		// Preserve records produced by callers still using the v1-v3 field.
		binaryIdentity = meta.BinaryLabel
	}
	provenance := make(map[string]string, len(meta.FieldProvenance)+len(derivedProvenanceKeys))
	for key, source := range meta.FieldProvenance {
		provenance[key] = source
	}
	// Runtime-derived EffectiveConfig members are computed downstream of the
	// profile resolver, so the record stamps them itself. A caller-supplied
	// source wins (verify_timeout is derived unless the CLI set it).
	for _, name := range derivedProvenanceKeys {
		if _, ok := provenance[name]; !ok {
			provenance[name] = "derived"
		}
	}
	rec := &ConfigRecord{
		SchemaVersion:           configRecordSchemaVersion,
		Binary:                  binaryIdentity,
		BinaryIdentity:          binaryIdentity,
		InvocationSurface:       meta.InvocationSurface,
		RequestedProtocol:       meta.RequestedProtocol,
		ProtocolFallbackReason:  meta.ProtocolFallbackReason,
		PromptSHA256:            sha256Hex([]byte(systemPrompt)),
		ToolSchemaSHA256:        jsonSHA256(toolRepresentation),
		ConfigSHA256:            spec.ConfigSHA256(),
		EffectiveProtocol:       effectiveProtocol,
		Policy:                  &pol,
		ResolutionTrace:         &trace,
		ResolutionTraceSHA256:   trace.TraceSHA256(),
		RuntimeResolutions:      append([]RuntimeResolution(nil), vs.events...),
		RuntimeResolutionSHA256: jsonSHA256(vs.events),
		Bindings:                runtimeBindingsOf(rt),
		ApprovalPolicyName:      meta.ApprovalPolicyName,
		ApprovalPolicyHash:      meta.ApprovalPolicyHash,
		ExecProfileHash:         meta.ExecProfileHash,
		CLIOverrides:            append([]string(nil), meta.CLIOverrides...),
		RequiredTrust:           meta.RequiredTrust,
		Canonical:               meta.Canonical,
		FieldProvenance:         provenance,
	}
	if meta.TrustProfile != "" {
		trustProfile := meta.TrustProfile
		rec.TrustProfile = &trustProfile
	}
	if meta.ExecProfileName != "" {
		execProfile := meta.ExecProfileName
		rec.ExecProfile = &execProfile
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rec.HarnessCommit = strings.TrimSpace(s.Value)
			case "vcs.modified":
				rec.HarnessDirty, _ = strconv.ParseBool(s.Value)
			}
		}
	}
	return rec
}

// effectiveConfigFromSpec is the S6a record projection: the SAME EffectiveConfig
// shape (schema v8, byte-identical goldens) built from the resolved spec and
// the held runtime/verify state instead of a lazy re-derivation from Config.
// The verification fields deliberately read the MUTABLE verifyState — the
// historical record captured the post-auto-verify values, and S6c will replace
// this with the canonical policy-value + runtime-resolution serializations.
func effectiveConfigFromSpec(pol runspec.PolicyValue, rt Runtime, vs *verifyState, effectiveProtocol string) EffectiveConfig {
	return EffectiveConfig{
		EffectiveProtocol:  effectiveProtocol,
		DisableMemoryStore: pol.DisableMemoryStore, Persona: pol.Persona, MemoryScope: pol.MemoryScope,
		BootContext: pol.BootContext, StandingContext: pol.StandingContext, Stream: pol.Stream, MinIsolation: pol.MinIsolation, RequireNetworkOff: pol.RequireNetworkOff,
		MaxIterations: pol.MaxIterations, MaxTokens: pol.MaxTokens, RunTimeout: pol.RunTimeout, VerifyTimeout: vs.Timeout,
		ReasoningEffort: pol.ReasoningEffort, PromptProfile: pol.PromptProfile, CodeAct: pol.CodeAct, ReproFirst: pol.ReproFirst,
		ReproGate: pol.ReproGate, BatchReads: pol.BatchReads, ReadWindow: pol.ReadWindow, ReadOutline: pol.ReadOutline,
		MaxWallClock: pol.MaxWallClock, MaxTotalTokens: pol.MaxTotalTokens, MaxTotalCostUSD: pol.MaxTotalCostUSD,
		AllowUnpricedSpend: pol.AllowUnpricedSpend, SolverModel: pol.SolverModel,
		ModelConfigured: rt.Model != nil, MemoryConfigured: rt.Memory != nil, VerifySandboxConfigured: rt.VerifySandbox != nil,
		CostFnConfigured: rt.CostFn != nil, SpendConfigured: rt.Spend != nil, ModelInfo: rt.ModelInfo,
		ReviewerIdentity: runtimeImplementationIdentity(rt.Reviewer), PlannerIdentity: runtimeImplementationIdentity(rt.Planner),
		VerifyCmd: vs.Cmd, AutoVerify: vs.AutoVerify,
		AutoVerifySoft: vs.AutoVerifySoft, SkipVerifyBaseline: vs.SkipVerifyBaseline, AbortOnRedBaseline: vs.AbortOnRedBaseline,
		VerifyLastRun: vs.VerifyLastRun, ChurnNudgeRuns: pol.ChurnNudgeRuns, VerifyContinue: vs.VerifyContinue,
		TestFence: pol.TestFence, DiffScope: pol.DiffScope, RequireDiff: pol.RequireDiff, ReviewConfigured: rt.Reviewer != nil,
		ReviewPolicy: pol.ReviewPolicy, ReviewUnverified: pol.ReviewUnverified, ReviewRounds: pol.ReviewRounds,
		PlannerConfigured: rt.Planner != nil, FinishNudgeWindow: pol.FinishNudgeWindow, DiagnoseCmd: pol.DiagnoseCmd,
		DiagnoseAfterEdits: pol.DiagnoseAfterEdits, NavSpiralWindow: pol.TerminationPolicy.NavSpiralWindow, TerminationPolicy: pol.TerminationPolicy, AnswerNudgeWindow: pol.AnswerNudgeWindow,
		FinishTool: pol.FinishTool, FinishToolConfigured: strings.TrimSpace(pol.FinishTool) != "", FinishToolTrustsCaller: pol.FinishToolTrustsCaller,
	}
}

// EffectiveConfigFromSpec is the exported golden-test projection for a spec +
// runtime pair OUTSIDE a running loop (pre-run verification state, native
// protocol).
func EffectiveConfigFromSpec(spec runspec.ResolvedSpec, rt Runtime) EffectiveConfig {
	pol := spec.Policy()
	return effectiveConfigFromSpec(pol, rt, newVerifyState(pol), "tools")
}

// configFieldClass is an external completeness anchor for Config. Keep this table
// separate from EffectiveConfig: adding an exported Config field must make an
// explicit recording/redaction decision rather than silently changing the hash.
type configFieldClass struct {
	class          string
	representation string // EffectiveConfig or ConfigRecord field for recorded entries.
}

const (
	recordedDirect  = "recorded-direct"
	recordedDerived = "recorded-derived"
	excludedRuntime = "excluded-runtime"
	excludedSecret  = "excluded-secret"
	excludedContent = "excluded-content"
)

// derivedProvenanceKeys are the EffectiveConfig JSON keys whose values are
// computed downstream of the profile resolver (class D in PROFILES.md
// Appendix A terms). field_provenance thus uses two disjoint key namespaces:
// resolver FieldIDs (e.g. "max_iters") for resolution-time fields, and these
// EffectiveConfig JSON keys for derived fields. A conformance test pins each
// entry to a real EffectiveConfig JSON tag so the list cannot drift.
var derivedProvenanceKeys = []string{
	"effective_protocol", "memory_scope", "min_isolation", "require_network_off", "verify_timeout",
	"model_info", "model_configured", "memory_configured",
	"verify_sandbox_configured", "cost_fn_configured", "spend_configured",
	"reviewer_identity", "planner_identity", "review_configured",
	"planner_configured", "finish_tool_configured",
}

var configFieldClasses = map[string]configFieldClass{
	"BinaryLabel": {recordedDerived, "ConfigRecord.Binary"}, "BinaryIdentity": {recordedDerived, "ConfigRecord.BinaryIdentity"}, "InvocationSurface": {recordedDerived, "ConfigRecord.InvocationSurface"},
	"RequestedProtocol": {recordedDerived, "ConfigRecord.RequestedProtocol"}, "ProtocolFallbackReason": {recordedDerived, "ConfigRecord.ProtocolFallbackReason"},
	"TrustProfile": {recordedDerived, "ConfigRecord.TrustProfile"}, "ExecProfileName": {recordedDerived, "ConfigRecord.ExecProfile"}, "ExecProfileHash": {recordedDerived, "ConfigRecord.ExecProfileHash"}, "CLIOverrides": {recordedDerived, "ConfigRecord.CLIOverrides"}, "RequiredTrust": {recordedDerived, "ConfigRecord.RequiredTrust"}, "Canonical": {recordedDerived, "ConfigRecord.Canonical"}, "FieldProvenance": {recordedDerived, "ConfigRecord.FieldProvenance"}, "ApprovalPolicyName": {recordedDerived, "ConfigRecord.ApprovalPolicyName"}, "ApprovalPolicyHash": {recordedDerived, "ConfigRecord.ApprovalPolicyHash"},
	"Model": {recordedDerived, "EffectiveConfig.ModelConfigured"}, "Sandbox": {excludedRuntime, ""}, "Memory": {recordedDerived, "EffectiveConfig.MemoryConfigured"}, "DisableMemoryStore": {recordedDirect, "DisableMemoryStore"}, "Persona": {recordedDirect, "Persona"}, "MemoryScope": {recordedDirect, "MemoryScope"}, "VerifySandbox": {recordedDerived, "EffectiveConfig.VerifySandboxConfigured"}, "Tools": {recordedDerived, "ConfigRecord.ToolSchemaSHA256"},
	"Task": {excludedContent, ""}, "TaskImages": {excludedContent, ""}, "History": {excludedContent, ""}, "Root": {excludedContent, ""}, "BootContext": {recordedDirect, "BootContext"}, "StandingContext": {recordedDirect, "StandingContext"}, "Obs": {excludedRuntime, ""}, "PerfMark": {excludedRuntime, ""}, "ModelInfo": {recordedDirect, "ModelInfo"}, "Stream": {recordedDirect, "Stream"}, "MinIsolation": {recordedDirect, "MinIsolation"}, "RequireNetworkOff": {recordedDirect, "RequireNetworkOff"},
	"MaxIterations": {recordedDirect, "MaxIterations"}, "MaxTokens": {recordedDirect, "MaxTokens"}, "RunTimeout": {recordedDirect, "RunTimeout"}, "VerifyTimeout": {recordedDirect, "VerifyTimeout"}, "ReasoningEffort": {recordedDirect, "ReasoningEffort"}, "PromptProfile": {recordedDirect, "PromptProfile"}, "CodeAct": {recordedDirect, "CodeAct"}, "ReproFirst": {recordedDirect, "ReproFirst"}, "ReproGate": {recordedDirect, "ReproGate"}, "BatchReads": {recordedDirect, "BatchReads"}, "ReadWindow": {recordedDirect, "ReadWindow"}, "ReadOutline": {recordedDirect, "ReadOutline"}, "MaxWallClock": {recordedDirect, "MaxWallClock"}, "MaxTotalTokens": {recordedDirect, "MaxTotalTokens"}, "MaxTotalCostUSD": {recordedDirect, "MaxTotalCostUSD"}, "AllowUnpricedSpend": {recordedDirect, "AllowUnpricedSpend"}, "CostFn": {recordedDerived, "EffectiveConfig.CostFnConfigured"}, "SolverModel": {recordedDirect, "SolverModel"}, "Spend": {recordedDerived, "EffectiveConfig.SpendConfigured"},
	"VerifyCmd": {recordedDirect, "VerifyCmd"}, "AutoVerify": {recordedDirect, "AutoVerify"}, "AutoVerifySoft": {recordedDirect, "AutoVerifySoft"}, "SkipVerifyBaseline": {recordedDirect, "SkipVerifyBaseline"}, "AbortOnRedBaseline": {recordedDirect, "AbortOnRedBaseline"}, "VerifyLastRun": {recordedDirect, "VerifyLastRun"}, "ChurnNudgeRuns": {recordedDirect, "ChurnNudgeRuns"}, "VerifyContinue": {recordedDirect, "VerifyContinue"}, "TestFence": {recordedDirect, "TestFence"}, "DiffScope": {recordedDirect, "DiffScope"}, "Reviewer": {recordedDerived, "EffectiveConfig.ReviewerIdentity"}, "ReviewPolicy": {recordedDirect, "ReviewPolicy"}, "RequireDiff": {recordedDirect, "RequireDiff"}, "ReviewUnverified": {recordedDirect, "ReviewUnverified"}, "ReviewRounds": {recordedDirect, "ReviewRounds"}, "Planner": {recordedDerived, "EffectiveConfig.PlannerIdentity"}, "FinishNudgeWindow": {recordedDirect, "FinishNudgeWindow"}, "DiagnoseCmd": {recordedDirect, "DiagnoseCmd"}, "DiagnoseAfterEdits": {recordedDirect, "DiagnoseAfterEdits"}, "TerminationPolicy": {recordedDerived, "EffectiveConfig.TerminationPolicy"}, "AnswerNudgeWindow": {recordedDirect, "AnswerNudgeWindow"}, "FinishTool": {recordedDirect, "FinishTool"}, "FinishToolTrustsCaller": {recordedDirect, "FinishToolTrustsCaller"},
}

// checkConfigFieldClassifications is shared by the oracle and its negative test.
func checkConfigFieldClassifications(configType reflect.Type, classes map[string]configFieldClass) []string {
	var problems []string
	for i := 0; i < configType.NumField(); i++ {
		field := configType.Field(i)
		if field.PkgPath != "" { // unexported implementation state
			continue
		}
		entry, ok := classes[field.Name]
		if !ok {
			problems = append(problems, "exported Config field "+field.Name+" has no classification")
			continue
		}
		if entry.class != recordedDirect && entry.class != recordedDerived && entry.class != excludedRuntime && entry.class != excludedSecret && entry.class != excludedContent {
			problems = append(problems, "Config field "+field.Name+" has invalid class "+entry.class)
		}
		if entry.class == recordedDirect || entry.class == recordedDerived {
			parts := strings.Split(entry.representation, ".")
			t := reflect.TypeOf(EffectiveConfig{})
			name := entry.representation
			if len(parts) == 2 {
				name = parts[1]
				if parts[0] == "ConfigRecord" {
					t = reflect.TypeOf(ConfigRecord{})
				} else if parts[0] != "EffectiveConfig" {
					problems = append(problems, "Config field "+field.Name+" has invalid representation "+entry.representation)
					continue
				}
			}
			if _, ok := t.FieldByName(name); !ok {
				problems = append(problems, "Config field "+field.Name+" claims missing representation "+entry.representation)
			}
		}
	}
	for name := range classes {
		if _, ok := configType.FieldByName(name); !ok {
			problems = append(problems, "stale Config classification for "+name)
		}
	}
	return problems
}

func runtimeImplementationIdentity(v any) string {
	if v == nil {
		return ""
	}
	return reflect.TypeOf(v).String()
}

func jsonSHA256(v any) string {
	b, _ := json.Marshal(v)
	return sha256Hex(b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
