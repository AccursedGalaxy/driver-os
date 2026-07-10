package agent

// TerminationPolicy is the versioned collection of thresholds used by the
// no-progress detectors. A recorded policy is always fully resolved.
type TerminationPolicy struct {
	Version              string `json:"version"`
	MaxRepeats           int    `json:"max_repeats"`
	MaxReasoningRepeats  int    `json:"max_reasoning_repeats"`
	MaxStagnant          int    `json:"max_stagnant"`
	NavSpiralWindow      int    `json:"nav_spiral_window"`
	WanderMultiple       int    `json:"wander_multiple"`
	FrontierCap          int    `json:"frontier_cap"`
	GreenRepeatThreshold int    `json:"green_repeat_threshold"`
}

func DefaultTerminationPolicy() TerminationPolicy {
	return TerminationPolicy{Version: "tp-2026-07-10", MaxRepeats: maxRepeats, MaxReasoningRepeats: maxReasoningRepeats, MaxStagnant: maxStagnant, NavSpiralWindow: noProgressWindow, WanderMultiple: spiralWanderMultiple, FrontierCap: spiralFrontierCap, GreenRepeatThreshold: 3}
}

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
	// The long-standing Config knob is an explicit compatibility override and wins.
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
