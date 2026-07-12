package agent

// The S6a eager==lazy equivalence pin, post-S6b: the lazy resolution twin
// (resolveKnobs) is deleted, so the pin states the expected resolved values
// EXPLICITLY — same table, same semantics, now against literals instead of
// the legacy function.

import (
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/runspec"
)

func rsInt(v int) *int                                                { return &v }
func rsDuration(v time.Duration) *time.Duration                       { return &v }
func rsPolicy(v runspec.TerminationPolicy) *runspec.TerminationPolicy { return &v }

func TestRunSpecMatchesLegacyLazyResolution(t *testing.T) {
	fullPolicy := runspec.TerminationPolicy{
		Version: "test", MaxRepeats: 7, MaxReasoningRepeats: 8, MaxStagnant: 9,
		NavSpiralWindow: 10, WanderMultiple: 11, FrontierCap: 12, GreenRepeatThreshold: 13,
	}
	def := runspec.DefaultTerminationPolicy()
	partial := def
	partial.MaxRepeats = 7
	aliased := def
	aliased.NavSpiralWindow = 7
	for _, tc := range []struct {
		name       string
		r          runspec.RequestedConfig
		wantKnobs  runspec.Knobs
		wantPolicy runspec.TerminationPolicy
	}{
		{"defaults", runspec.RequestedConfig{},
			runspec.Knobs{MaxIterations: runspec.DefaultMaxIterations, MaxTokens: runspec.DefaultMaxTokens, RunTimeout: runspec.DefaultRunTimeout, SpiralWindow: def.NavSpiralWindow}, def},
		{"max iterations", runspec.RequestedConfig{MaxIterations: rsInt(17)},
			runspec.Knobs{MaxIterations: 17, MaxTokens: runspec.DefaultMaxTokens, RunTimeout: runspec.DefaultRunTimeout, SpiralWindow: def.NavSpiralWindow}, def},
		{"tokens and timeout", runspec.RequestedConfig{MaxTokens: rsInt(4096), RunTimeout: rsDuration(45 * time.Second)},
			runspec.Knobs{MaxIterations: runspec.DefaultMaxIterations, MaxTokens: 4096, RunTimeout: 45 * time.Second, SpiralWindow: def.NavSpiralWindow}, def},
		{"full policy", runspec.RequestedConfig{TerminationPolicy: rsPolicy(fullPolicy)},
			runspec.Knobs{MaxIterations: runspec.DefaultMaxIterations, MaxTokens: runspec.DefaultMaxTokens, RunTimeout: runspec.DefaultRunTimeout, SpiralWindow: 10}, fullPolicy},
		{"partial policy", runspec.RequestedConfig{TerminationPolicy: rsPolicy(runspec.TerminationPolicy{MaxRepeats: 7})},
			runspec.Knobs{MaxIterations: runspec.DefaultMaxIterations, MaxTokens: runspec.DefaultMaxTokens, RunTimeout: runspec.DefaultRunTimeout, SpiralWindow: def.NavSpiralWindow}, partial},
		{"alias only", runspec.RequestedConfig{NavSpiralWindow: rsInt(7)},
			runspec.Knobs{MaxIterations: runspec.DefaultMaxIterations, MaxTokens: runspec.DefaultMaxTokens, RunTimeout: runspec.DefaultRunTimeout, SpiralWindow: 7}, aliased},
		{"alias with unset policy", runspec.RequestedConfig{NavSpiralWindow: rsInt(7), TerminationPolicy: rsPolicy(runspec.TerminationPolicy{})},
			runspec.Knobs{MaxIterations: runspec.DefaultMaxIterations, MaxTokens: runspec.DefaultMaxTokens, RunTimeout: runspec.DefaultRunTimeout, SpiralWindow: 7}, aliased},
		{"review rounds", runspec.RequestedConfig{ReviewRounds: rsInt(4)},
			runspec.Knobs{MaxIterations: runspec.DefaultMaxIterations, MaxTokens: runspec.DefaultMaxTokens, RunTimeout: runspec.DefaultRunTimeout, SpiralWindow: def.NavSpiralWindow}, def},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, _, err := runspec.Resolve(tc.r)
			if err != nil {
				t.Fatal(err)
			}
			if got := spec.Knobs(); got != tc.wantKnobs {
				t.Fatalf("Knobs() = %#v, want %#v", got, tc.wantKnobs)
			}
			if got := spec.Termination(); got != tc.wantPolicy {
				t.Fatalf("Termination() = %#v, want %#v", got, tc.wantPolicy)
			}
		})
	}
}
