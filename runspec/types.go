// Package runspec resolves presence-preserving run policy requests into complete,
// immutable policy values. It deliberately has no dependency on package agent.
package runspec

import "time"

const (
	DefaultMaxIterations = 8
	DefaultMaxTokens     = 8192
	DefaultRunTimeout    = 30 * time.Second
	DefaultReviewRounds  = 2
)

// TerminationPolicy is the versioned set of no-progress thresholds.
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
	return TerminationPolicy{Version: "tp-2026-07-10", MaxRepeats: 2, MaxReasoningRepeats: 6, MaxStagnant: 3, NavSpiralWindow: 4, WanderMultiple: 4, FrontierCap: 64, GreenRepeatThreshold: 3}
}
