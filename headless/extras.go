package headless

import (
	"context"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
)

// PriceLookup returns the modeled cost for a model and usage.
type PriceLookup func(model string, usage llm.Usage) (float64, bool)

// LadderConfig is an opaque ladder configuration owned by the wiring package.
type LadderConfig interface{}

// LadderRequest contains only core types needed to execute a ladder.
type LadderRequest struct {
	Config    LadderConfig
	Base      agent.Config
	Dir       string
	Run       func(context.Context, agent.Config) (*agent.RunResult, error)
	Provider  func(string) (llm.Provider, error)
	Reviewer  func(string) (agent.Reviewer, error)
	Price     PriceLookup
	RecordDir string
	Task      string
}

// LadderResult is the headless projection of an orchestration result.
type LadderResult struct {
	RunResult   *agent.RunResult
	WinnerModel string
	RecordErr   error
}

// Extras are optional capabilities supplied by a fully armed binary.
type Extras struct {
	Price       PriceLookup
	NewReviewer func(llm.Provider, string, string) agent.Reviewer
	NewPlanner  func(llm.Provider, string, string) agent.Planner
	LoadLadder  func(string) (LadderConfig, error)
	RunLadder   func(context.Context, LadderRequest) (*LadderResult, error)
}
