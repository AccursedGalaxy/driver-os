package headless

import (
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/memory"
	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/runspec"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// baseConfigInput contains the values resolved by the headless CLI before a
// run. Keeping construction here makes that wiring testable without CLI, git,
// environment, or provider setup.
type baseConfigInput struct {
	InvocationSurface, RequestedProtocol, FallbackReason                  string
	ExecProfile                                                           profile.Exec
	CLIOverrides                                                          []string
	Model                                                                 llm.Provider
	ModelInfo                                                             llm.ModelInfo
	Sandbox, VerifySandbox                                                sandbox.Sandbox
	Memory                                                                memory.Store
	Tools                                                                 map[string]agent.Tool
	Task, Root                                                            string
	MinIsolation                                                          sandbox.Isolation
	RequireNetworkOff                                                     bool
	TrustProfile, RequiredTrust                                           string
	Obs                                                                   agent.Observer
	Canonical                                                             bool
	FieldProvenance                                                       map[string]string
	MaxIterations, MaxTokens                                              int
	RunTimeout, VerifyTimeout                                             time.Duration
	VerifyCmd                                                             string
	SkipVerifyBaseline, AbortOnRedBaseline, VerifyLastRun, VerifyContinue bool
	TestFence, DiffScope                                                  []string
	Reviewer                                                              agent.Reviewer
	ReviewPolicy                                                          agent.ReviewPolicy
	ReviewUnverified, RequireDiff                                         bool
	ReviewRounds                                                          int
	Planner                                                               agent.Planner
	ReasoningEffort, PromptProfile                                        string
	CodeAct, ReproFirst, ReproGate, BatchReads, BootContext               bool
	ChurnNudgeRuns                                                        int
	MaxWallClock                                                          time.Duration
	MaxTotalTokens, FinishNudgeWindow                                     int
	DiagnoseCmd                                                           string
	DiagnoseAfterEdits                                                    int
	AutoVerify, AutoVerifySoft, StandingContext                           bool
	NavSpiralWindow, AnswerNudgeWindow                                    int
}

func buildBaseConfig(in baseConfigInput) agent.Config {
	return agent.Config{
		BinaryIdentity: agent.BinaryIdentityDriver, InvocationSurface: in.InvocationSurface, RequestedProtocol: in.RequestedProtocol, ProtocolFallbackReason: in.FallbackReason,
		ExecProfileName: in.ExecProfile.Name, ExecProfileHash: in.ExecProfile.Hash(), CLIOverrides: in.CLIOverrides, Model: in.Model,
		ModelInfo: in.ModelInfo, Sandbox: in.Sandbox, VerifySandbox: in.VerifySandbox, Memory: in.Memory, Tools: in.Tools, Task: in.Task, Root: in.Root,
		MinIsolation: in.MinIsolation, RequireNetworkOff: in.RequireNetworkOff, TrustProfile: in.TrustProfile, Obs: in.Obs, RequiredTrust: in.RequiredTrust,
		Canonical: in.Canonical, FieldProvenance: in.FieldProvenance, MaxIterations: in.MaxIterations, MaxTokens: in.MaxTokens, RunTimeout: in.RunTimeout,
		VerifyTimeout: in.VerifyTimeout, VerifyCmd: in.VerifyCmd, SkipVerifyBaseline: in.SkipVerifyBaseline, AbortOnRedBaseline: in.AbortOnRedBaseline,
		VerifyLastRun: in.VerifyLastRun, VerifyContinue: in.VerifyContinue, TestFence: in.TestFence, DiffScope: in.DiffScope, Reviewer: in.Reviewer,
		ReviewPolicy: in.ReviewPolicy, ReviewUnverified: in.ReviewUnverified, RequireDiff: in.RequireDiff, ReviewRounds: in.ReviewRounds, Planner: in.Planner,
		ReasoningEffort: in.ReasoningEffort, PromptProfile: in.PromptProfile, CodeAct: in.CodeAct, ReproFirst: in.ReproFirst, ReproGate: in.ReproGate,
		BatchReads: in.BatchReads, BootContext: in.BootContext, ChurnNudgeRuns: in.ChurnNudgeRuns, MaxWallClock: in.MaxWallClock,
		MaxTotalTokens: in.MaxTotalTokens, FinishNudgeWindow: in.FinishNudgeWindow, DiagnoseCmd: in.DiagnoseCmd, DiagnoseAfterEdits: in.DiagnoseAfterEdits,
		AutoVerify: in.AutoVerify, AutoVerifySoft: in.AutoVerifySoft, StandingContext: in.StandingContext,
		TerminationPolicy: agent.TerminationPolicy{NavSpiralWindow: in.NavSpiralWindow}, AnswerNudgeWindow: in.AnswerNudgeWindow,
	}
}

// S6d.2s — the native request path. buildBaseRequest produces a
// runspec.RequestedConfig straight from flags, so runspec.Resolve (inside
// agent.Prepare) owns profile threading end to end — the flag layer no longer
// calls profile.Resolve itself and hands a pre-resolved Config to Split(). This
// is what collapses the two disagreeing provenance views today's ConfigRecord
// carries (PROFILES.md §7.5 S6d).
//
// PRESENCE (the load-bearing rule, pinned by TestBaseRequestEquivalentToSplit):
//   - Profile-covered fields (profile.ProfileFields) ride in Overrides with
//     fs.Visit presence — set IFF the operator supplied the flag. Absent → the
//     forwarded profile supplies the default. This is sound only because the
//     flag defaults are themselves profile-seeded (headless/flags.go
//     registerFlagsWithProfile), so unset ≡ profile default and the run stays
//     canonical. Reproducing legacy optNZ value-presence here would resurrect a
//     profile default over an explicit -flag=false (agent.Prepare's contract
//     test spells out why).
//   - Non-profile fields carry value-presence (nzp): zero ≡ unset, matching
//     Config.Requested()'s optNZ so a bare run resolves byte-identically.
type baseRequestInput struct {
	TrustProfile    string
	ExecProfileName string
	// Overrides carries the profile-covered flags the operator explicitly set
	// (headless's profileOverrides(fs, f) output) — fs.Visit presence.
	Overrides profile.Overrides
	// Non-profile policy flags (value-presence).
	VerifyTimeout                                         time.Duration
	VerifyCmd                                             string
	SkipVerifyBaseline, AbortOnRedBaseline, VerifyLastRun bool
	ReviewPolicy                                          agent.ReviewPolicy
	ReviewUnverified                                      bool
	ReviewRounds                                          int
	TestFence, DiffScope                                  []string
	PromptProfile                                         string
	CodeAct, ReproFirst, ReproGate                        bool
	MaxWallClock                                          time.Duration
	MaxTotalTokens                                        int
	MaxTotalCostUSD                                       float64
	AllowUnpricedSpend                                    bool
	DiagnoseCmd                                           string
	DiagnoseAfterEdits                                    int
	SolverModel                                           string
	EffectiveProtocol                                     string
}

// nzp (non-zero pointer) mirrors agent.optNZ: value-presence for non-profile
// fields, so a bare run reaches runspec.Resolve with exactly the shape
// Config.Requested() produced.
func nzp[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

func buildBaseRequest(in baseRequestInput) runspec.RequestedConfig {
	o := in.Overrides
	r := runspec.RequestedConfig{
		TrustProfile: &in.TrustProfile,
		// Profile-covered fields: fs.Visit presence, straight from Overrides.
		MaxIterations:     o.MaxIters,
		MaxTokens:         o.MaxTokens,
		FinishNudgeWindow: o.FinishNudge,
		RunTimeout:        o.RunTimeout,
		ReasoningEffort:   o.Effort,
		Worktree:          o.Worktree,
		VerifyContinue:    o.VerifyContinue,
		AutoVerify:        o.AutoVerify,
		AutoVerifySoft:    o.AutoVerifySoft,
		Memory:            o.Memory,
		BootContext:       o.BootContext,
		StandingContext:   o.StandingContext,
		BatchReads:        o.BatchReads,
		RequireDiff:       o.RequireDiff,
		ReadWindow:        o.ReadWindow,
		ReadOutline:       o.ReadOutline,
		ChurnNudgeRuns:    o.ChurnNudgeRuns,
		NavSpiralWindow:   o.NavSpiralWindow,
		AnswerNudgeWindow: o.AnswerNudgeWindow,
		// Non-profile fields: value-presence (nzp), matching optNZ.
		EffectiveProtocol:  nzp(in.EffectiveProtocol),
		VerifyTimeout:      nzp(in.VerifyTimeout),
		VerifyCmd:          nzp(in.VerifyCmd),
		SkipVerifyBaseline: nzp(in.SkipVerifyBaseline),
		AbortOnRedBaseline: nzp(in.AbortOnRedBaseline),
		VerifyLastRun:      nzp(in.VerifyLastRun),
		ReviewPolicy:       nzp(int(in.ReviewPolicy)),
		ReviewUnverified:   nzp(in.ReviewUnverified),
		ReviewRounds:       nzp(in.ReviewRounds),
		PromptProfile:      nzp(in.PromptProfile),
		CodeAct:            nzp(in.CodeAct),
		ReproFirst:         nzp(in.ReproFirst),
		ReproGate:          nzp(in.ReproGate),
		MaxWallClock:       nzp(in.MaxWallClock),
		MaxTotalTokens:     nzp(in.MaxTotalTokens),
		MaxTotalCostUSD:    nzp(in.MaxTotalCostUSD),
		AllowUnpricedSpend: nzp(in.AllowUnpricedSpend),
		DiagnoseCmd:        nzp(in.DiagnoseCmd),
		DiagnoseAfterEdits: nzp(in.DiagnoseAfterEdits),
		SolverModel:        nzp(in.SolverModel),
	}
	if in.ExecProfileName != "" {
		r.ExecProfileName = &in.ExecProfileName
	}
	if len(in.TestFence) > 0 {
		fence := append([]string(nil), in.TestFence...)
		r.TestFence = &fence
	}
	if len(in.DiffScope) > 0 {
		scope := append([]string(nil), in.DiffScope...)
		r.DiffScope = &scope
	}
	return r
}
