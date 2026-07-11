package agent

import (
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/internal/runspec"
)

func rsInt(v int) *int                                                { return &v }
func rsDuration(v time.Duration) *time.Duration                       { return &v }
func rsPolicy(v runspec.TerminationPolicy) *runspec.TerminationPolicy { return &v }

func TestRunSpecMatchesLegacyLazyResolution(t *testing.T) {
	fullPolicy := runspec.TerminationPolicy{
		Version: "test", MaxRepeats: 7, MaxReasoningRepeats: 8, MaxStagnant: 9,
		NavSpiralWindow: 10, WanderMultiple: 11, FrontierCap: 12, GreenRepeatThreshold: 13,
	}
	for _, tc := range []struct {
		name string
		r    runspec.RequestedConfig
		cfg  Config
	}{
		{"defaults", runspec.RequestedConfig{}, Config{}},
		{"max iterations", runspec.RequestedConfig{MaxIterations: rsInt(17)}, Config{MaxIterations: 17}},
		{"tokens and timeout", runspec.RequestedConfig{MaxTokens: rsInt(4096), RunTimeout: rsDuration(45 * time.Second)}, Config{MaxTokens: 4096, RunTimeout: 45 * time.Second}},
		{"full policy", runspec.RequestedConfig{TerminationPolicy: rsPolicy(fullPolicy)}, Config{TerminationPolicy: fullPolicy}},
		{"partial policy", runspec.RequestedConfig{TerminationPolicy: rsPolicy(runspec.TerminationPolicy{MaxRepeats: 7})}, Config{TerminationPolicy: TerminationPolicy{MaxRepeats: 7}}},
		{"alias only", runspec.RequestedConfig{NavSpiralWindow: rsInt(7)}, Config{NavSpiralWindow: 7}},
		{"alias with unset policy", runspec.RequestedConfig{NavSpiralWindow: rsInt(7), TerminationPolicy: rsPolicy(runspec.TerminationPolicy{})}, Config{NavSpiralWindow: 7, TerminationPolicy: TerminationPolicy{}}},
		{"review rounds", runspec.RequestedConfig{ReviewRounds: rsInt(4)}, Config{ReviewRounds: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, _, err := runspec.Resolve(tc.r)
			if err != nil {
				t.Fatal(err)
			}
			got := spec.Knobs()
			want := resolveKnobs(tc.cfg)
			if got.MaxIterations != want.maxIter || got.MaxTokens != want.maxTok || got.RunTimeout != want.runTimeout || got.SpiralWindow != want.spiralWindow {
				t.Fatalf("Knobs() = %#v, legacy = %#v", got, want)
			}
			if got := spec.Termination(); got != want.terminationPolicy {
				t.Fatalf("Termination() = %#v, legacy = %#v", got, want.terminationPolicy)
			}
		})
	}
}
