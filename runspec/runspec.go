package runspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/memory"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// RequestedConfig is the transport-neutral, presence-preserving policy input.
type RequestedConfig struct {
	TrustProfile                                                                      *string
	ExecProfileName                                                                   *string
	EffectiveProtocol                                                                 *string
	DisableMemoryStore                                                                *bool
	Memory                                                                            *bool
	Persona                                                                           *string
	MemoryScope                                                                       *memory.Scope
	BootContext, StandingContext, Stream                                              *bool
	MinIsolation                                                                      *sandbox.Isolation
	RequireNetworkOff                                                                 *bool
	MaxIterations, MaxTokens                                                          *int
	RunTimeout, VerifyTimeout                                                         *time.Duration
	ReasoningEffort, PromptProfile                                                    *string
	CodeAct, ReproFirst, ReproGate, BatchReads                                        *bool
	ReadWindow                                                                        *int
	ReadOutline                                                                       *bool
	MaxWallClock                                                                      *time.Duration
	MaxTotalTokens                                                                    *int
	MaxTotalCostUSD                                                                   *float64
	AllowUnpricedSpend                                                                *bool
	SolverModel                                                                       *string
	VerifyCmd                                                                         *string
	AutoVerify, AutoVerifySoft, SkipVerifyBaseline, AbortOnRedBaseline, VerifyLastRun *bool
	ChurnNudgeRuns                                                                    *int
	VerifyContinue                                                                    *bool
	TestFence, DiffScope                                                              *[]string
	ReviewPolicy                                                                      *int
	RequireDiff, ReviewUnverified                                                     *bool
	ReviewRounds                                                                      *int
	FinishNudgeWindow                                                                 *int
	DiagnoseCmd                                                                       *string
	DiagnoseAfterEdits                                                                *int
	NavSpiralWindow                                                                   *int // compatibility alias
	TerminationPolicy                                                                 *TerminationPolicy
	AnswerNudgeWindow                                                                 *int
	FinishTool                                                                        *string
	FinishToolTrustsCaller                                                            *bool
	Worktree                                                                          *string
}

// PolicyValue is the complete canonical policy projection.
type PolicyValue struct {
	EffectiveProtocol      string            `json:"effective_protocol"`
	DisableMemoryStore     bool              `json:"disable_memory_store"`
	Memory                 bool              `json:"memory"`
	Persona                string            `json:"persona,omitempty"`
	MemoryScope            memory.Scope      `json:"memory_scope"`
	BootContext            bool              `json:"boot_context"`
	StandingContext        bool              `json:"standing_context"`
	Stream                 bool              `json:"stream"`
	MinIsolation           sandbox.Isolation `json:"min_isolation"`
	RequireNetworkOff      bool              `json:"require_network_off"`
	MaxIterations          int               `json:"max_iterations"`
	MaxTokens              int               `json:"max_tokens"`
	RunTimeout             time.Duration     `json:"run_timeout"`
	VerifyTimeout          time.Duration     `json:"verify_timeout"`
	ReasoningEffort        string            `json:"reasoning_effort,omitempty"`
	PromptProfile          string            `json:"prompt_profile,omitempty"`
	CodeAct                bool              `json:"code_act"`
	ReproFirst             bool              `json:"repro_first"`
	ReproGate              bool              `json:"repro_gate"`
	BatchReads             bool              `json:"batch_reads"`
	ReadWindow             int               `json:"read_window"`
	ReadOutline            bool              `json:"read_outline"`
	MaxWallClock           time.Duration     `json:"max_wall_clock"`
	MaxTotalTokens         int               `json:"max_total_tokens"`
	MaxTotalCostUSD        float64           `json:"max_total_cost_usd"`
	AllowUnpricedSpend     bool              `json:"allow_unpriced_spend"`
	SolverModel            string            `json:"solver_model,omitempty"`
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
	ReviewPolicy           int               `json:"review_policy"`
	RequireDiff            bool              `json:"require_diff"`
	ReviewUnverified       bool              `json:"review_unverified"`
	ReviewRounds           int               `json:"review_rounds"`
	FinishNudgeWindow      int               `json:"finish_nudge_window"`
	DiagnoseCmd            string            `json:"diagnose_cmd,omitempty"`
	DiagnoseAfterEdits     int               `json:"diagnose_after_edits"`
	TerminationPolicy      TerminationPolicy `json:"termination_policy"`
	AnswerNudgeWindow      int               `json:"answer_nudge_window"`
	FinishTool             string            `json:"finish_tool,omitempty"`
	FinishToolTrustsCaller bool              `json:"finish_tool_trusts_caller"`
	Worktree               string            `json:"worktree"`
}

type ResolvedSpec struct {
	policy    PolicyValue
	canonical []byte
	hash      string
	trace     Trace
}
type TraceEntry struct {
	Source string `json:"source"`
	Value  any    `json:"value"`
}
type Trace struct {
	Fields       map[string]TraceEntry `json:"fields"`
	TrustProfile string                `json:"trust_profile"`
	ExecProfile  string                `json:"exec_profile,omitempty"`
	Canonical    bool                  `json:"canonical"`
	// Profile is the authentic per-field profile-resolution trace (the §3 seam's
	// own record) when an execution profile was named — the source binaries use
	// for transcript FieldProvenance/Canonical/RequiredTrust. Zero when no
	// profile was requested. Not part of the trace digest (S6c owns unifying
	// the two serializations).
	Profile   profile.Trace `json:"-"`
	canonical []byte
	hash      string
}

// overridesFrom projects the profile-covered subset of a RequestedConfig into
// profile.Overrides, preserving presence. This is what makes Resolve an
// EXTENSION of the existing profile seam rather than a parallel one
// (PROFILES.md §7.2): profile.Resolve sees the same explicit flags the
// binaries used to hand it, so its trace and floor checks stay authentic.
func overridesFrom(r RequestedConfig) profile.Overrides {
	return profile.Overrides{
		MaxIters: r.MaxIterations, MaxTokens: r.MaxTokens, FinishNudge: r.FinishNudgeWindow,
		RunTimeout: r.RunTimeout, Effort: r.ReasoningEffort, Worktree: r.Worktree,
		VerifyContinue: r.VerifyContinue, AutoVerify: r.AutoVerify, AutoVerifySoft: r.AutoVerifySoft,
		Memory: r.Memory, BootContext: r.BootContext, StandingContext: r.StandingContext,
		BatchReads: r.BatchReads, RequireDiff: r.RequireDiff, ReadWindow: r.ReadWindow,
		ReadOutline: r.ReadOutline, ChurnNudgeRuns: r.ChurnNudgeRuns,
		NavSpiralWindow: r.NavSpiralWindow, AnswerNudgeWindow: r.AnswerNudgeWindow,
	}
}

type Knobs struct {
	MaxIterations int
	MaxTokens     int
	RunTimeout    time.Duration
	SpiralWindow  int
}
type Verification struct {
	VerifyCmd                                                                                         string
	VerifyTimeout                                                                                     time.Duration
	SkipVerifyBaseline, AbortOnRedBaseline, VerifyLastRun, VerifyContinue, AutoVerify, AutoVerifySoft bool
}
type Review struct {
	Rounds      int
	Policy      int
	Unverified  bool
	RequireDiff bool
}

func (s ResolvedSpec) Knobs() Knobs {
	return Knobs{s.policy.MaxIterations, s.policy.MaxTokens, s.policy.RunTimeout, s.policy.TerminationPolicy.NavSpiralWindow}
}
func (s ResolvedSpec) Termination() TerminationPolicy { return s.policy.TerminationPolicy }
func (s ResolvedSpec) Verification() Verification {
	p := s.policy
	return Verification{p.VerifyCmd, p.VerifyTimeout, p.SkipVerifyBaseline, p.AbortOnRedBaseline, p.VerifyLastRun, p.VerifyContinue, p.AutoVerify, p.AutoVerifySoft}
}
func (s ResolvedSpec) Review() Review {
	p := s.policy
	return Review{p.ReviewRounds, p.ReviewPolicy, p.ReviewUnverified, p.RequireDiff}
}
func (s ResolvedSpec) Policy() PolicyValue {
	p := s.policy
	p.TestFence = append([]string(nil), p.TestFence...)
	p.DiffScope = append([]string(nil), p.DiffScope...)
	return p
}
func (s ResolvedSpec) ConfigSHA256() string { return s.hash }

// CanonicalPolicy returns the canonical policy-value serialization captured at
// construction — the bytes ConfigSHA256 hashes (PROFILES.md §7.3).
func (s ResolvedSpec) CanonicalPolicy() []byte { return append([]byte(nil), s.canonical...) }

// Trace returns the resolution trace captured with this spec (per-field
// provenance travels WITH the spec but serializes separately — §7.1/§7.3).
func (s ResolvedSpec) Trace() Trace { return s.trace }

func (t Trace) TraceSHA256() string { return t.hash }

func digest(v any) ([]byte, string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, "", e
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), nil
}
func mark(t *Trace, name, source string, value any) { t.Fields[name] = TraceEntry{source, value} }
func set[T any](dst *T, src *T, t *Trace, name string) {
	if src != nil {
		*dst = *src
		mark(t, name, "override", *src)
		t.Canonical = false
	}
}

func resolveTermination(p *TerminationPolicy) TerminationPolicy {
	d := DefaultTerminationPolicy()
	if p == nil {
		return d
	}
	r := *p
	if r.Version == "" {
		r.Version = d.Version
	}
	if r.MaxRepeats <= 0 {
		r.MaxRepeats = d.MaxRepeats
	}
	if r.MaxReasoningRepeats <= 0 {
		r.MaxReasoningRepeats = d.MaxReasoningRepeats
	}
	if r.MaxStagnant <= 0 {
		r.MaxStagnant = d.MaxStagnant
	}
	if r.NavSpiralWindow <= 0 {
		r.NavSpiralWindow = d.NavSpiralWindow
	}
	if r.WanderMultiple <= 0 {
		r.WanderMultiple = d.WanderMultiple
	}
	if r.FrontierCap <= 0 {
		r.FrontierCap = d.FrontierCap
	}
	if r.GreenRepeatThreshold <= 0 {
		r.GreenRepeatThreshold = d.GreenRepeatThreshold
	}
	return r
}

// validatePolicy is the per-field resolved-value schema (each field carries
// its own valid range and disable semantics — zero legitimately means
// "disabled" for some windows/counts, so this is never a blanket <= 0 rule).
func validatePolicy(p PolicyValue) error {
	checks := []struct {
		name string
		bad  bool
	}{{"MaxIterations", p.MaxIterations <= 0}, {"MaxTokens", p.MaxTokens <= 0}, {"RunTimeout", p.RunTimeout <= 0}, {"VerifyTimeout", p.VerifyTimeout <= 0}, {"ReviewRounds", p.ReviewRounds <= 0}, {"ReadWindow", p.ReadWindow < 0}, {"ChurnNudgeRuns", p.ChurnNudgeRuns < 0}, {"FinishNudgeWindow", p.FinishNudgeWindow < 0}, {"AnswerNudgeWindow", p.AnswerNudgeWindow < 0}, {"TerminationPolicy.NavSpiralWindow", p.TerminationPolicy.NavSpiralWindow <= 0}}
	for _, c := range checks {
		if c.bad {
			return fmt.Errorf("%s: value out of range", c.name)
		}
	}
	return nil
}

// Complete asserts that this spec was produced by Resolve and still satisfies
// every per-field invariant. The loops call it at entry and may only ASSERT
// completeness, never repair it (PROFILES.md §7.2): the zero ResolvedSpec —
// the only non-Resolve value a caller can construct, since storage is
// unexported — fails here before any environmental work.
func (s ResolvedSpec) Complete() error {
	if s.hash == "" {
		return fmt.Errorf("runspec: incomplete ResolvedSpec (zero value) — construct it with Resolve")
	}
	return validatePolicy(s.policy)
}

// Resolve applies profile declarations, overrides, aliases, trust floors, then validation.
func Resolve(r RequestedConfig) (ResolvedSpec, Trace, error) {
	p := PolicyValue{MaxIterations: DefaultMaxIterations, MaxTokens: DefaultMaxTokens, RunTimeout: DefaultRunTimeout, ReviewRounds: DefaultReviewRounds, TerminationPolicy: DefaultTerminationPolicy()}
	t := Trace{Fields: map[string]TraceEntry{}, Canonical: true}
	trust := profile.TrustedLocal
	if r.TrustProfile != nil {
		x, e := profile.ParseTrust(*r.TrustProfile)
		if e != nil {
			return ResolvedSpec{}, t, e
		}
		trust = x
	}
	t.TrustProfile = string(trust)
	floor := profile.FloorFor(trust)
	p.MinIsolation = floor.MinIsolation
	p.RequireNetworkOff = floor.Network == profile.NetworkOff
	if floor.Worktree == profile.WorktreeRequired {
		p.Worktree = "required"
	} else {
		p.Worktree = "auto"
	}
	if r.ExecProfileName != nil && *r.ExecProfileName != "" {
		exec, err := profile.ExecByName(*r.ExecProfileName)
		if err != nil {
			return ResolvedSpec{}, t, err
		}
		t.ExecProfile = exec.Name
		n, tr, err := profile.Resolve(profile.TrustPosture{Selected: trust, Posture: profile.FloorFor(trust)}, exec, overridesFrom(r))
		if err != nil {
			return ResolvedSpec{}, t, err
		}
		t.Profile = tr
		p.MaxIterations = n.MaxIters
		p.MaxTokens = n.MaxTokens
		p.FinishNudgeWindow = n.FinishNudge
		p.RunTimeout = n.RunTimeout
		p.ReasoningEffort = n.Effort
		p.Worktree = n.Worktree
		p.VerifyContinue = n.VerifyContinue
		p.AutoVerify = n.AutoVerify
		p.AutoVerifySoft = n.AutoVerifySoft
		p.Memory = n.Memory
		p.BootContext = n.BootContext
		p.StandingContext = n.StandingContext
		p.BatchReads = n.BatchReads
		p.RequireDiff = n.RequireDiff
		p.ReadWindow = n.ReadWindow
		p.ReadOutline = n.ReadOutline
		p.ChurnNudgeRuns = n.ChurnNudgeRuns
		p.TerminationPolicy.NavSpiralWindow = n.NavSpiralWindow
		p.AnswerNudgeWindow = n.AnswerNudgeWindow
		for k, v := range tr.Fields {
			mark(&t, k, "profile default", v.Value)
		}
	}
	set(&p.EffectiveProtocol, r.EffectiveProtocol, &t, "EffectiveProtocol")
	set(&p.DisableMemoryStore, r.DisableMemoryStore, &t, "DisableMemoryStore")
	set(&p.Memory, r.Memory, &t, "Memory")
	set(&p.Persona, r.Persona, &t, "Persona")
	set(&p.MemoryScope, r.MemoryScope, &t, "MemoryScope")
	set(&p.BootContext, r.BootContext, &t, "BootContext")
	set(&p.StandingContext, r.StandingContext, &t, "StandingContext")
	set(&p.Stream, r.Stream, &t, "Stream")
	set(&p.MinIsolation, r.MinIsolation, &t, "MinIsolation")
	set(&p.RequireNetworkOff, r.RequireNetworkOff, &t, "RequireNetworkOff")
	set(&p.MaxIterations, r.MaxIterations, &t, "MaxIterations")
	set(&p.MaxTokens, r.MaxTokens, &t, "MaxTokens")
	set(&p.RunTimeout, r.RunTimeout, &t, "RunTimeout")
	set(&p.VerifyTimeout, r.VerifyTimeout, &t, "VerifyTimeout")
	set(&p.ReasoningEffort, r.ReasoningEffort, &t, "ReasoningEffort")
	set(&p.PromptProfile, r.PromptProfile, &t, "PromptProfile")
	set(&p.CodeAct, r.CodeAct, &t, "CodeAct")
	set(&p.ReproFirst, r.ReproFirst, &t, "ReproFirst")
	set(&p.ReproGate, r.ReproGate, &t, "ReproGate")
	set(&p.BatchReads, r.BatchReads, &t, "BatchReads")
	set(&p.ReadWindow, r.ReadWindow, &t, "ReadWindow")
	set(&p.ReadOutline, r.ReadOutline, &t, "ReadOutline")
	set(&p.MaxWallClock, r.MaxWallClock, &t, "MaxWallClock")
	set(&p.MaxTotalTokens, r.MaxTotalTokens, &t, "MaxTotalTokens")
	set(&p.MaxTotalCostUSD, r.MaxTotalCostUSD, &t, "MaxTotalCostUSD")
	set(&p.AllowUnpricedSpend, r.AllowUnpricedSpend, &t, "AllowUnpricedSpend")
	set(&p.SolverModel, r.SolverModel, &t, "SolverModel")
	set(&p.VerifyCmd, r.VerifyCmd, &t, "VerifyCmd")
	set(&p.AutoVerify, r.AutoVerify, &t, "AutoVerify")
	set(&p.AutoVerifySoft, r.AutoVerifySoft, &t, "AutoVerifySoft")
	set(&p.SkipVerifyBaseline, r.SkipVerifyBaseline, &t, "SkipVerifyBaseline")
	set(&p.AbortOnRedBaseline, r.AbortOnRedBaseline, &t, "AbortOnRedBaseline")
	set(&p.VerifyLastRun, r.VerifyLastRun, &t, "VerifyLastRun")
	set(&p.ChurnNudgeRuns, r.ChurnNudgeRuns, &t, "ChurnNudgeRuns")
	set(&p.VerifyContinue, r.VerifyContinue, &t, "VerifyContinue")
	if r.TestFence != nil {
		p.TestFence = append([]string(nil), (*r.TestFence)...)
		mark(&t, "TestFence", "override", p.TestFence)
	}
	if r.DiffScope != nil {
		p.DiffScope = append([]string(nil), (*r.DiffScope)...)
		mark(&t, "DiffScope", "override", p.DiffScope)
	}
	set(&p.ReviewPolicy, r.ReviewPolicy, &t, "ReviewPolicy")
	set(&p.RequireDiff, r.RequireDiff, &t, "RequireDiff")
	set(&p.ReviewUnverified, r.ReviewUnverified, &t, "ReviewUnverified")
	set(&p.ReviewRounds, r.ReviewRounds, &t, "ReviewRounds")
	set(&p.FinishNudgeWindow, r.FinishNudgeWindow, &t, "FinishNudgeWindow")
	set(&p.DiagnoseCmd, r.DiagnoseCmd, &t, "DiagnoseCmd")
	set(&p.DiagnoseAfterEdits, r.DiagnoseAfterEdits, &t, "DiagnoseAfterEdits")
	set(&p.AnswerNudgeWindow, r.AnswerNudgeWindow, &t, "AnswerNudgeWindow")
	set(&p.FinishTool, r.FinishTool, &t, "FinishTool")
	set(&p.FinishToolTrustsCaller, r.FinishToolTrustsCaller, &t, "FinishToolTrustsCaller")
	set(&p.Worktree, r.Worktree, &t, "Worktree")
	if r.TerminationPolicy != nil {
		for _, c := range []struct {
			name  string
			value int
		}{
			{"TerminationPolicy.MaxRepeats", r.TerminationPolicy.MaxRepeats},
			{"TerminationPolicy.MaxReasoningRepeats", r.TerminationPolicy.MaxReasoningRepeats},
			{"TerminationPolicy.MaxStagnant", r.TerminationPolicy.MaxStagnant},
			{"TerminationPolicy.NavSpiralWindow", r.TerminationPolicy.NavSpiralWindow},
			{"TerminationPolicy.WanderMultiple", r.TerminationPolicy.WanderMultiple},
			{"TerminationPolicy.FrontierCap", r.TerminationPolicy.FrontierCap},
			{"TerminationPolicy.GreenRepeatThreshold", r.TerminationPolicy.GreenRepeatThreshold},
		} {
			if c.value < 0 {
				return ResolvedSpec{}, t, fmt.Errorf("%s: value out of range", c.name)
			}
		}
	}
	if r.NavSpiralWindow != nil && *r.NavSpiralWindow < 0 {
		return ResolvedSpec{}, t, fmt.Errorf("NavSpiralWindow: value out of range")
	}
	if r.TerminationPolicy != nil {
		p.TerminationPolicy = resolveTermination(r.TerminationPolicy)
		mark(&t, "TerminationPolicy", "override", *r.TerminationPolicy)
		t.Canonical = false
	} else {
		p.TerminationPolicy = resolveTermination(&p.TerminationPolicy)
	}
	if r.NavSpiralWindow != nil {
		if r.TerminationPolicy != nil && *r.NavSpiralWindow > 0 && r.TerminationPolicy.NavSpiralWindow > 0 && *r.NavSpiralWindow != r.TerminationPolicy.NavSpiralWindow {
			return ResolvedSpec{}, t, fmt.Errorf("NavSpiralWindow conflicts with TerminationPolicy.NavSpiralWindow")
		}
		p.TerminationPolicy.NavSpiralWindow = *r.NavSpiralWindow
		mark(&t, "TerminationPolicy.NavSpiralWindow", "alias", *r.NavSpiralWindow)
		t.Canonical = false
	}
	candidate := profile.FloorFor(trust)
	candidate.MinIsolation = p.MinIsolation
	candidate.Network = profile.NetworkUnrestricted
	if p.RequireNetworkOff {
		candidate.Network = profile.NetworkOff
	}
	if p.Worktree == "required" {
		candidate.Worktree = profile.WorktreeRequired
	}
	if err := profile.Enforce(trust, candidate); err != nil {
		return ResolvedSpec{}, t, err
	}
	mark(&t, "trust", "floor", trust)
	if r.MaxIterations == nil && p.MaxIterations == 0 {
		p.MaxIterations = DefaultMaxIterations
	}
	if r.MaxTokens == nil && p.MaxTokens == 0 {
		p.MaxTokens = DefaultMaxTokens
	}
	if r.RunTimeout == nil && p.RunTimeout == 0 {
		p.RunTimeout = DefaultRunTimeout
	}
	if r.ReviewRounds == nil && p.ReviewRounds == 0 {
		p.ReviewRounds = DefaultReviewRounds
	}
	if p.TerminationPolicy.NavSpiralWindow == 0 {
		p.TerminationPolicy.NavSpiralWindow = DefaultTerminationPolicy().NavSpiralWindow
	}
	if p.VerifyTimeout == 0 {
		p.VerifyTimeout = 5 * time.Minute
		if p.RunTimeout > p.VerifyTimeout {
			p.VerifyTimeout = p.RunTimeout
		}
	}
	if err := validatePolicy(p); err != nil {
		return ResolvedSpec{}, t, err
	}
	b, h, e := digest(p)
	if e != nil {
		return ResolvedSpec{}, t, e
	}
	s := ResolvedSpec{policy: p, canonical: b, hash: h}
	tb, th, e := digest(struct {
		Fields       map[string]TraceEntry `json:"fields"`
		TrustProfile string                `json:"trust_profile"`
		ExecProfile  string                `json:"exec_profile,omitempty"`
		Canonical    bool                  `json:"canonical"`
	}{t.Fields, t.TrustProfile, t.ExecProfile, t.Canonical})
	if e != nil {
		return ResolvedSpec{}, t, e
	}
	t.canonical, t.hash = tb, th
	s.trace = t
	return s, t, nil
}
