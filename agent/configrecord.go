package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/mneme"
)

// v2: trust identity + approval policy identity.
const configRecordSchemaVersion = 2

// ConfigRecord is the reproducibility record embedded in transcripts. It records
// today's effective config plus stable hashes; ExecProfile remains a reserved
// seam for later profile resolution slices.
type ConfigRecord struct {
	SchemaVersion      int             `json:"schema_version"`
	Binary             string          `json:"binary,omitempty"`
	HarnessCommit      string          `json:"harness_commit,omitempty"`
	HarnessDirty       bool            `json:"harness_dirty,omitempty"`
	PromptSHA256       string          `json:"prompt_sha256"`
	ToolSchemaSHA256   string          `json:"tool_schema_sha256"`
	ConfigSHA256       string          `json:"config_sha256"`
	Effective          EffectiveConfig `json:"effective"`
	TrustProfile       *string         `json:"trust_profile"`
	ExecProfile        *string         `json:"exec_profile"`
	ApprovalPolicyName string          `json:"approval_policy_name,omitempty"`
	ApprovalPolicyHash string          `json:"approval_policy_hash,omitempty"`
}

// EffectiveConfig is the serializable projection of Config fields that affect
// agent behavior. Runtime objects and task content are deliberately excluded.
type EffectiveConfig struct {
	DisableMemoryStore bool              `json:"disable_memory_store"`
	Persona            string            `json:"persona,omitempty"`
	MemoryScope        mneme.Scope       `json:"memory_scope"`
	BootContext        bool              `json:"boot_context"`
	StandingContext    bool              `json:"standing_context"`
	Stream             bool              `json:"stream"`
	MinIsolation       sandbox.Isolation `json:"min_isolation"`
	MaxIterations      int               `json:"max_iterations"`
	MaxTokens          int               `json:"max_tokens"`
	RunTimeout         time.Duration     `json:"run_timeout"`
	VerifyTimeout      time.Duration     `json:"verify_timeout"`
	ReasoningEffort    string            `json:"reasoning_effort,omitempty"`
	PromptProfile      string            `json:"prompt_profile,omitempty"`
	CodeAct            bool              `json:"code_act"`
	ReproFirst         bool              `json:"repro_first"`
	ReproGate          bool              `json:"repro_gate"`
	BatchReads         bool              `json:"batch_reads"`
	ReadWindow         int               `json:"read_window"`
	ReadOutline        bool              `json:"read_outline"`
	MaxWallClock       time.Duration     `json:"max_wall_clock"`
	MaxTotalTokens     int               `json:"max_total_tokens"`
	MaxTotalCostUSD    float64           `json:"max_total_cost_usd"`
	AllowUnpricedSpend bool              `json:"allow_unpriced_spend"`
	SolverModel        string            `json:"solver_model,omitempty"`

	VerifyCmd              string   `json:"verify_cmd,omitempty"`
	AutoVerify             bool     `json:"auto_verify"`
	AutoVerifySoft         bool     `json:"auto_verify_soft"`
	SkipVerifyBaseline     bool     `json:"skip_verify_baseline"`
	AbortOnRedBaseline     bool     `json:"abort_on_red_baseline"`
	VerifyLastRun          bool     `json:"verify_last_run"`
	ChurnNudgeRuns         int      `json:"churn_nudge_runs"`
	VerifyContinue         bool     `json:"verify_continue"`
	TestFence              []string `json:"test_fence,omitempty"`
	DiffScope              []string `json:"diff_scope,omitempty"`
	RequireDiff            bool     `json:"require_diff"`
	ReviewConfigured       bool     `json:"review_configured"`
	ReviewPolicy           int      `json:"review_policy"`
	ReviewUnverified       bool     `json:"review_unverified"`
	ReviewRounds           int      `json:"review_rounds"`
	PlannerConfigured      bool     `json:"planner_configured"`
	FinishNudgeWindow      int      `json:"finish_nudge_window"`
	DiagnoseCmd            string   `json:"diagnose_cmd,omitempty"`
	DiagnoseAfterEdits     int      `json:"diagnose_after_edits"`
	NavSpiralWindow        int      `json:"nav_spiral_window"`
	AnswerNudgeWindow      int      `json:"answer_nudge_window"`
	FinishToolConfigured   bool     `json:"finish_tool_configured"`
	FinishToolTrustsCaller bool     `json:"finish_tool_trusts_caller"`
}

func newConfigRecord(cfg Config, systemPrompt string, schemas []llm.Tool) *ConfigRecord {
	eff := effectiveConfig(cfg)
	rec := &ConfigRecord{
		SchemaVersion:      configRecordSchemaVersion,
		Binary:             cfg.BinaryLabel,
		PromptSHA256:       sha256Hex([]byte(systemPrompt)),
		ToolSchemaSHA256:   jsonSHA256(schemas),
		ConfigSHA256:       jsonSHA256(eff),
		Effective:          eff,
		ApprovalPolicyName: cfg.ApprovalPolicyName,
		ApprovalPolicyHash: cfg.ApprovalPolicyHash,
	}
	if cfg.TrustProfile != "" {
		trustProfile := cfg.TrustProfile
		rec.TrustProfile = &trustProfile
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

func effectiveConfig(cfg Config) EffectiveConfig {
	knobs := resolveKnobs(cfg)
	maxIter, maxTok, runTimeout, spiralWindow := knobs.maxIter, knobs.maxTok, knobs.runTimeout, knobs.spiralWindow
	reviewRounds := cfg.ReviewRounds
	if reviewRounds <= 0 {
		reviewRounds = DefaultReviewRounds
	}
	return EffectiveConfig{
		DisableMemoryStore: cfg.DisableMemoryStore, Persona: cfg.Persona, MemoryScope: cfg.MemoryScope,
		BootContext: cfg.BootContext, StandingContext: cfg.StandingContext, Stream: cfg.Stream, MinIsolation: cfg.MinIsolation,
		MaxIterations: maxIter, MaxTokens: maxTok, RunTimeout: runTimeout, VerifyTimeout: verifyTimeout(cfg, runTimeout),
		ReasoningEffort: cfg.ReasoningEffort, PromptProfile: cfg.PromptProfile, CodeAct: cfg.CodeAct, ReproFirst: cfg.ReproFirst,
		ReproGate: cfg.ReproGate, BatchReads: cfg.BatchReads, ReadWindow: cfg.ReadWindow, ReadOutline: cfg.ReadOutline,
		MaxWallClock: cfg.MaxWallClock, MaxTotalTokens: cfg.MaxTotalTokens, MaxTotalCostUSD: cfg.MaxTotalCostUSD,
		AllowUnpricedSpend: cfg.AllowUnpricedSpend, SolverModel: cfg.SolverModel, VerifyCmd: cfg.VerifyCmd, AutoVerify: cfg.AutoVerify,
		AutoVerifySoft: cfg.AutoVerifySoft, SkipVerifyBaseline: cfg.SkipVerifyBaseline, AbortOnRedBaseline: cfg.AbortOnRedBaseline,
		VerifyLastRun: cfg.VerifyLastRun, ChurnNudgeRuns: cfg.ChurnNudgeRuns, VerifyContinue: cfg.VerifyContinue,
		TestFence: cfg.TestFence, DiffScope: cfg.DiffScope, RequireDiff: cfg.RequireDiff, ReviewConfigured: cfg.Reviewer != nil,
		ReviewPolicy: int(cfg.ReviewPolicy), ReviewUnverified: cfg.ReviewUnverified, ReviewRounds: reviewRounds,
		PlannerConfigured: cfg.Planner != nil, FinishNudgeWindow: cfg.FinishNudgeWindow, DiagnoseCmd: cfg.DiagnoseCmd,
		DiagnoseAfterEdits: cfg.DiagnoseAfterEdits, NavSpiralWindow: spiralWindow, AnswerNudgeWindow: cfg.AnswerNudgeWindow,
		FinishToolConfigured: strings.TrimSpace(cfg.FinishTool) != "", FinishToolTrustsCaller: cfg.FinishToolTrustsCaller,
	}
}

func jsonSHA256(v any) string {
	b, _ := json.Marshal(v)
	return sha256Hex(b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
