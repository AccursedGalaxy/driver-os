package agent

// S6a: agent.Config survives ONLY as an input shape on the REQUESTED side,
// consumed by runspec.Resolve — never seen by the loops (PROFILES.md §7.5).
// This file is the one canonical Config → (RequestedConfig, Runtime, Content)
// split. Presence semantics are the historical lazy ones: a zero-valued policy
// field means "unset" (Resolve fills the default, which the equivalence tests
// pin byte-identical to the old lazy resolution); explicit-zero is not
// expressible through Config — callers that need it construct RequestedConfig
// directly. ExecProfileName/TrustProfile are deliberately NOT forwarded to
// resolution: a Config carries already-resolved values (its constructors ran
// the profile/trust machinery), so re-applying profile defaults over zero
// fields would resurrect them. They ride in RecordMeta, recording-only.
//
// Deleted with Config in S6b once every construction path builds
// RequestedConfig natively.

import (
	"github.com/AccursedGalaxy/driver-os/memory"
	"github.com/AccursedGalaxy/driver-os/runspec"
)

func optNZ[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// Requested projects cfg's policy fields into a presence-preserving
// RequestedConfig (zero value = unset, historical lazy semantics).
func (c Config) Requested() runspec.RequestedConfig {
	r := runspec.RequestedConfig{
		DisableMemoryStore:     optNZ(c.DisableMemoryStore),
		Persona:                optNZ(c.Persona),
		BootContext:            optNZ(c.BootContext),
		StandingContext:        optNZ(c.StandingContext),
		Stream:                 optNZ(c.Stream),
		MinIsolation:           optNZ(c.MinIsolation),
		RequireNetworkOff:      optNZ(c.RequireNetworkOff),
		MaxIterations:          optNZ(c.MaxIterations),
		MaxTokens:              optNZ(c.MaxTokens),
		RunTimeout:             optNZ(c.RunTimeout),
		VerifyTimeout:          optNZ(c.VerifyTimeout),
		ReasoningEffort:        optNZ(c.ReasoningEffort),
		PromptProfile:          optNZ(c.PromptProfile),
		CodeAct:                optNZ(c.CodeAct),
		ReproFirst:             optNZ(c.ReproFirst),
		ReproGate:              optNZ(c.ReproGate),
		BatchReads:             optNZ(c.BatchReads),
		ReadWindow:             optNZ(c.ReadWindow),
		ReadOutline:            optNZ(c.ReadOutline),
		MaxWallClock:           optNZ(c.MaxWallClock),
		MaxTotalTokens:         optNZ(c.MaxTotalTokens),
		MaxTotalCostUSD:        optNZ(c.MaxTotalCostUSD),
		AllowUnpricedSpend:     optNZ(c.AllowUnpricedSpend),
		SolverModel:            optNZ(c.SolverModel),
		VerifyCmd:              optNZ(c.VerifyCmd),
		AutoVerify:             optNZ(c.AutoVerify),
		AutoVerifySoft:         optNZ(c.AutoVerifySoft),
		SkipVerifyBaseline:     optNZ(c.SkipVerifyBaseline),
		AbortOnRedBaseline:     optNZ(c.AbortOnRedBaseline),
		VerifyLastRun:          optNZ(c.VerifyLastRun),
		ChurnNudgeRuns:         optNZ(c.ChurnNudgeRuns),
		VerifyContinue:         optNZ(c.VerifyContinue),
		ReviewPolicy:           optNZ(int(c.ReviewPolicy)),
		RequireDiff:            optNZ(c.RequireDiff),
		ReviewUnverified:       optNZ(c.ReviewUnverified),
		ReviewRounds:           optNZ(c.ReviewRounds),
		FinishNudgeWindow:      optNZ(c.FinishNudgeWindow),
		DiagnoseCmd:            optNZ(c.DiagnoseCmd),
		DiagnoseAfterEdits:     optNZ(c.DiagnoseAfterEdits),
		AnswerNudgeWindow:      optNZ(c.AnswerNudgeWindow),
		FinishTool:             optNZ(c.FinishTool),
		FinishToolTrustsCaller: optNZ(c.FinishToolTrustsCaller),
	}
	if c.MemoryScope != (memory.Scope{}) {
		scope := c.MemoryScope
		r.MemoryScope = &scope
	}
	if len(c.TestFence) > 0 {
		fence := c.TestFence
		r.TestFence = &fence
	}
	if len(c.DiffScope) > 0 {
		scope := c.DiffScope
		r.DiffScope = &scope
	}
	if c.TerminationPolicy != (TerminationPolicy{}) {
		tp := c.TerminationPolicy
		r.TerminationPolicy = &tp
	}
	return r
}

// Bindings projects cfg's runtime dependencies and per-run content.
func (c Config) Bindings() (Runtime, Content) {
	rt := Runtime{
		Model:         c.Model,
		Sandbox:       c.Sandbox,
		VerifySandbox: c.VerifySandbox,
		Memory:        c.Memory,
		Tools:         c.Tools,
		Obs:           c.Obs,
		PerfMark:      c.PerfMark,
		ModelInfo:     c.ModelInfo,
		CostFn:        c.CostFn,
		Spend:         c.Spend,
		Reviewer:      c.Reviewer,
		Planner:       c.Planner,
		Record: RecordMeta{
			BinaryLabel:            c.BinaryLabel,
			BinaryIdentity:         c.BinaryIdentity,
			InvocationSurface:      c.InvocationSurface,
			RequestedProtocol:      c.RequestedProtocol,
			ProtocolFallbackReason: c.ProtocolFallbackReason,
			TrustProfile:           c.TrustProfile,
			ExecProfileName:        c.ExecProfileName,
			ExecProfileHash:        c.ExecProfileHash,
			CLIOverrides:           c.CLIOverrides,
			RequiredTrust:          c.RequiredTrust,
			Canonical:              c.Canonical,
			FieldProvenance:        c.FieldProvenance,
			ApprovalPolicyName:     c.ApprovalPolicyName,
			ApprovalPolicyHash:     c.ApprovalPolicyHash,
		},
		verifyBaselineCache: c.verifyBaselineCache,
	}
	content := Content{Task: c.Task, TaskImages: c.TaskImages, History: c.History, Root: c.Root}
	return rt, content
}

// Split is Requested + Resolve + Bindings in one call — the whole requested-
// side conversion for callers that hold a Config template (eval, tests).
func (c Config) Split() (runspec.ResolvedSpec, Runtime, Content, error) {
	spec, _, err := runspec.Resolve(c.Requested())
	rt, content := c.Bindings()
	return spec, rt, content, err
}
