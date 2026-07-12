package runspec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/profile"
)

func intPtr(v int) *int                   { return &v }
func stringSlicePtr(v []string) *[]string { return &v }

func TestRequestedConfigCoversProfileFields(t *testing.T) {
	fieldNames := map[profile.FieldID]string{
		profile.MaxItersField:          "MaxIterations",
		profile.MaxTokensField:         "MaxTokens",
		profile.FinishNudgeField:       "FinishNudgeWindow",
		profile.RunTimeoutField:        "RunTimeout",
		profile.EffortField:            "ReasoningEffort",
		profile.WorktreeField:          "Worktree",
		profile.VerifyContinueField:    "VerifyContinue",
		profile.AutoVerifyField:        "AutoVerify",
		profile.AutoVerifySoftField:    "AutoVerifySoft",
		profile.MemoryField:            "Memory",
		profile.BootContextField:       "BootContext",
		profile.StandingContextField:   "StandingContext",
		profile.BatchReadsField:        "BatchReads",
		profile.RequireDiffField:       "RequireDiff",
		profile.ReadWindowField:        "ReadWindow",
		profile.ReadOutlineField:       "ReadOutline",
		profile.ChurnNudgeRunsField:    "ChurnNudgeRuns",
		profile.NavSpiralWindowField:   "NavSpiralWindow",
		profile.AnswerNudgeWindowField: "AnswerNudgeWindow",
	}
	requestType := reflect.TypeFor[RequestedConfig]()
	for _, id := range profile.ProfileFields {
		name, ok := fieldNames[id]
		if !ok {
			t.Fatalf("profile FieldID %q has no RequestedConfig mapping", id)
		}
		if _, ok := requestType.FieldByName(name); !ok {
			t.Fatalf("profile FieldID %q maps to missing RequestedConfig field %s", id, name)
		}
	}
}

func TestResolveNavSpiralWindowAlias(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    RequestedConfig
		want int
		err  bool
	}{
		{"conflict", RequestedConfig{NavSpiralWindow: intPtr(5), TerminationPolicy: &TerminationPolicy{NavSpiralWindow: 6}}, 0, true},
		{"equal", RequestedConfig{NavSpiralWindow: intPtr(5), TerminationPolicy: &TerminationPolicy{NavSpiralWindow: 5}}, 5, false},
		{"unset policy", RequestedConfig{NavSpiralWindow: intPtr(5), TerminationPolicy: &TerminationPolicy{}}, 5, false},
		{"alias only", RequestedConfig{NavSpiralWindow: intPtr(5)}, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, err := Resolve(tc.r)
			if tc.err {
				if err == nil || !strings.Contains(err.Error(), "NavSpiralWindow") {
					t.Fatalf("Resolve() error = %v, want NavSpiralWindow conflict", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Termination().NavSpiralWindow; got != tc.want {
				t.Fatalf("NavSpiralWindow = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResolveValidation(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		r           RequestedConfig
		wantErr     bool
	}{
		{"zero iterations", "MaxIterations", RequestedConfig{MaxIterations: intPtr(0)}, true},
		{"negative read window", "ReadWindow", RequestedConfig{ReadWindow: intPtr(-1)}, true},
		{"disabled answer nudge", "", RequestedConfig{AnswerNudgeWindow: intPtr(0)}, false},
		{"disabled finish nudge", "", RequestedConfig{FinishNudgeWindow: intPtr(0)}, false},
		{"disabled churn nudge", "", RequestedConfig{ChurnNudgeRuns: intPtr(0)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, err := Resolve(tc.r)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("Resolve() error = %v, want error naming %s", err, tc.field)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			p := s.Policy()
			if tc.name == "disabled answer nudge" && p.AnswerNudgeWindow != 0 || tc.name == "disabled finish nudge" && p.FinishNudgeWindow != 0 || tc.name == "disabled churn nudge" && p.ChurnNudgeRuns != 0 {
				t.Fatalf("disabled value was not preserved: %#v", p)
			}
		})
	}
}

func TestResolveSeparatesPolicyAndTraceHashes(t *testing.T) {
	name := "coding-v2"
	iterations := 40
	fromProfile, profileTrace, err := Resolve(RequestedConfig{ExecProfileName: &name})
	if err != nil {
		t.Fatal(err)
	}
	fromOverride, overrideTrace, err := Resolve(RequestedConfig{ExecProfileName: &name, MaxIterations: &iterations})
	if err != nil {
		t.Fatal(err)
	}
	if fromProfile.ConfigSHA256() != fromOverride.ConfigSHA256() {
		t.Fatal("equal effective policies have different config hashes")
	}
	if profileTrace.TraceSHA256() == overrideTrace.TraceSHA256() {
		t.Fatal("profile default and explicit override have identical trace hashes")
	}
}

func TestPolicyReturnsIndependentSlices(t *testing.T) {
	fence := []string{"*_test.go"}
	s, _, err := Resolve(RequestedConfig{TestFence: stringSlicePtr(fence)})
	if err != nil {
		t.Fatal(err)
	}
	p := s.Policy()
	p.TestFence[0] = "mutated"
	if got := s.Policy().TestFence[0]; got != "*_test.go" {
		t.Fatalf("TestFence mutated through Policy(): %q", got)
	}
}
