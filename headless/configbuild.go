package headless

import (
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/memory"
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
