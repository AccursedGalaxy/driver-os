package agent

import "github.com/AccursedGalaxy/driver-os/internal/runspec"

// TerminationPolicy remains source-compatible while its ownership lives in runspec.
type TerminationPolicy = runspec.TerminationPolicy

func DefaultTerminationPolicy() TerminationPolicy { return runspec.DefaultTerminationPolicy() }

func resolveTerminationPolicy(p TerminationPolicy, navOverride int) TerminationPolicy {
	d := DefaultTerminationPolicy()
	if p.Version == "" {
		p.Version = d.Version
	}
	if p.MaxRepeats <= 0 {
		p.MaxRepeats = d.MaxRepeats
	}
	if p.MaxReasoningRepeats <= 0 {
		p.MaxReasoningRepeats = d.MaxReasoningRepeats
	}
	if p.MaxStagnant <= 0 {
		p.MaxStagnant = d.MaxStagnant
	}
	if p.NavSpiralWindow <= 0 {
		p.NavSpiralWindow = d.NavSpiralWindow
	}
	if p.WanderMultiple <= 0 {
		p.WanderMultiple = d.WanderMultiple
	}
	if p.FrontierCap <= 0 {
		p.FrontierCap = d.FrontierCap
	}
	if p.GreenRepeatThreshold <= 0 {
		p.GreenRepeatThreshold = d.GreenRepeatThreshold
	}
	if navOverride > 0 {
		p.NavSpiralWindow = navOverride
	}
	return p
}

// DetectorCounters is write-only run telemetry. It must never affect control flow.
type DetectorCounters struct {
	Repeat           int    `json:"repeat"`
	ReasoningRepeat  int    `json:"reasoning_repeat"`
	ToolObsRepeat    int    `json:"tool_obs_repeat"`
	Stagnant         int    `json:"stagnant"`
	SpiralCycle      int    `json:"spiral_cycle"`
	SpiralWander     int    `json:"spiral_wander"`
	ChurnNudge       int    `json:"churn_nudge"`
	GreenRepeatNudge int    `json:"green_repeat_nudge"`
	FinishNudge      int    `json:"finish_nudge"`
	AnswerNudge      int    `json:"answer_nudge"`
	Diagnostics      int    `json:"diagnostics"`
	TerminatedBy     string `json:"terminated_by,omitempty"`
	TerminatedAtIter int    `json:"terminated_at_iter,omitempty"`
}

func (c *DetectorCounters) terminated(by string, iter int) {
	c.TerminatedBy, c.TerminatedAtIter = by, iter
}
