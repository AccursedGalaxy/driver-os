package headless

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/runspec"
)

// correctedFields are the two policy fields agent.Config cannot round-trip
// through Requested(), so the legacy Split path records their zero/floor value
// regardless of what the profile declared: "memory" (Config carries only the
// memory.Store binding, never a policy bool — so legacy always records false)
// and "worktree" (TrustProfile is withheld, so legacy always records the
// trusted-local floor, dropping any exec-profile demand). Both are
// recording-only — no loop, gate, or session consumes the policy field (the
// store binding and plan.ForceWorktree drive the real behavior) — so buildBase
// Request forwarding the profile CORRECTS the record without changing behavior.
// This is the same defect class as agent's TestPrepareCorrectsWorktreeFloor...,
// broadened: it moves ConfigSHA256 on essentially every headless run.
var correctedFields = map[string]bool{"memory": true, "worktree": true}

// policyDiff returns the JSON field keys on which two resolved policies differ.
func policyDiff(t *testing.T, a, b runspec.PolicyValue) []string {
	t.Helper()
	ma, mb := policyMap(t, a), policyMap(t, b)
	var diff []string
	for k, va := range ma {
		if string(va) != string(mb[k]) {
			diff = append(diff, k)
		}
	}
	return diff
}

func policyMap(t *testing.T, p runspec.PolicyValue) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The native request path (buildBaseRequest → agent.Prepare) must resolve to the
// SAME policy the legacy path (buildBaseConfig → Config.Split) does on every
// field the legacy path could faithfully represent — i.e. everything but the two
// correctedFields. This is the S6d.2s policy-equivalence gate. It mirrors the
// real main.go:688 field sourcing: profile-covered fields come from the resolved
// profile (fed as resolved values to buildBaseConfig; fed as Visit-presence
// Overrides to buildBaseRequest), non-profile fields from flag values.
//
// Trust is scoped to trusted-local / reviewed-local; container / untrusted add a
// worktree-floor correction pinned by agent's
// TestPrepareCorrectsWorktreeFloorUnderContainerTrust.
func TestBaseRequestEquivalentToSplit(t *testing.T) {
	profiles := []string{"coding-v2", "interactive-v2", "eval-swe-v1"}
	trusts := []profile.Trust{profile.TrustedLocal, profile.ReviewedLocal}

	cases := []struct {
		name string
		// ov are the profile-covered overrides (Visit-present flags).
		ov func() profile.Overrides
		// nonProfile mutates the shared non-profile flag values on both sides.
		nonProfile func(*baseRequestInput)
	}{
		{"bare", func() profile.Overrides { return profile.Overrides{} }, nil},
		{"max-iters", func() profile.Overrides { n := 17; return profile.Overrides{MaxIters: &n} }, nil},
		{"auto-verify-false", func() profile.Overrides { b := false; return profile.Overrides{AutoVerify: &b} }, nil},
		{"effort+standing", func() profile.Overrides {
			e, s := "high", true
			return profile.Overrides{Effort: &e, StandingContext: &s}
		}, nil},
		{"non-profile-flags", func() profile.Overrides { return profile.Overrides{} }, func(in *baseRequestInput) {
			in.VerifyCmd = "go test ./..."
			in.VerifyTimeout = 90 * time.Second
			in.ReviewRounds = 3
			in.TestFence = []string{"*_test.go", "testdata/**"}
			in.MaxTotalCostUSD = 0.05
			in.DiagnoseCmd = "go build ./..."
		}},
	}

	for _, pname := range profiles {
		e, err := profile.ExecByName(pname)
		if err != nil {
			t.Fatal(err)
		}
		for _, trust := range trusts {
			for _, tc := range cases {
				t.Run(pname+"/"+string(trust)+"/"+tc.name, func(t *testing.T) {
					ov := tc.ov()
					posture := profile.TrustPosture{Selected: trust, Posture: profile.FloorFor(trust), Surface: "headless"}
					resolved, _, err := profile.Resolve(posture, e, ov)
					if err != nil {
						t.Fatal(err)
					}
					floor := profile.FloorFor(trust)

					// Shared non-profile flag values.
					req := baseRequestInput{TrustProfile: string(trust), ExecProfileName: e.Name, Overrides: ov}
					if tc.nonProfile != nil {
						tc.nonProfile(&req)
					}

					// LEGACY: buildBaseConfig fed resolved profile values +
					// the floor values the flag layer bakes (main.go:692), then Split.
					legacyCfg := buildBaseConfig(baseConfigInput{
						InvocationSurface: agent.InvocationSurfaceDriverRun, ExecProfile: e,
						MinIsolation: floor.MinIsolation, RequireNetworkOff: floor.RequiresNetworkOff(), TrustProfile: string(trust),
						MaxIterations: resolved.MaxIters, MaxTokens: resolved.MaxTokens, RunTimeout: resolved.RunTimeout,
						VerifyContinue: resolved.VerifyContinue, RequireDiff: resolved.RequireDiff, ReasoningEffort: resolved.Effort,
						BatchReads: resolved.BatchReads, BootContext: resolved.BootContext, ChurnNudgeRuns: resolved.ChurnNudgeRuns,
						FinishNudgeWindow: resolved.FinishNudge, AutoVerify: resolved.AutoVerify, AutoVerifySoft: resolved.AutoVerifySoft,
						StandingContext: resolved.StandingContext, NavSpiralWindow: resolved.NavSpiralWindow, AnswerNudgeWindow: resolved.AnswerNudgeWindow,
						VerifyCmd: req.VerifyCmd, VerifyTimeout: req.VerifyTimeout, ReviewRounds: req.ReviewRounds,
						TestFence: req.TestFence, DiagnoseCmd: req.DiagnoseCmd,
					})
					legacyCfg.MaxTotalCostUSD = req.MaxTotalCostUSD
					legacySpec, _, _, err := legacyCfg.Split()
					if err != nil {
						t.Fatalf("Split: %v", err)
					}

					// NATIVE: buildBaseRequest → Prepare (profile forwarded).
					p, err := agent.Prepare(buildBaseRequest(req), agent.Runtime{}, agent.Content{}, agent.RecordInputs{})
					if err != nil {
						t.Fatalf("Prepare: %v", err)
					}

					for _, k := range policyDiff(t, legacySpec.Policy(), p.Spec().Policy()) {
						if !correctedFields[k] {
							t.Errorf("policy field %q diverged (not a documented correction)\n  Split:   %s\n  Prepare: %s",
								k, legacySpec.CanonicalPolicy(), p.Spec().CanonicalPolicy())
						}
					}
				})
			}
		}
	}
}

// The two corrections are asserted directly, so their expected direction is on
// the record (not merely allowlisted out of the equivalence oracle):
//   - memory: coding-v2 declares Memory=true, but Config carries only the store
//     binding, so legacy records false; Prepare records the profile's true.
//   - worktree: eval-swe-v1 DEMANDS off, but Config withholds the profile, so
//     legacy records the trusted-local floor "auto"; Prepare records "off".
//
// Both move ConfigSHA256; both are recording-only (behavior is driven by the
// store binding and plan.ForceWorktree). eval reproducibility is a headline
// goal, so the corrected eval-swe-v1 record matters most.
func TestBaseRequestCorrectsDroppedProfileFields(t *testing.T) {
	trust := profile.TrustedLocal
	prepare := func(t *testing.T, pname string) (legacy, native runspec.PolicyValue) {
		t.Helper()
		e, err := profile.ExecByName(pname)
		if err != nil {
			t.Fatal(err)
		}
		posture := profile.TrustPosture{Selected: trust, Posture: profile.FloorFor(trust), Surface: "headless"}
		resolved, _, err := profile.Resolve(posture, e, profile.Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		legacyCfg := buildBaseConfig(baseConfigInput{
			InvocationSurface: agent.InvocationSurfaceDriverRun, ExecProfile: e, TrustProfile: string(trust),
			MaxIterations: resolved.MaxIters, MaxTokens: resolved.MaxTokens, RunTimeout: resolved.RunTimeout,
		})
		legacySpec, _, _, err := legacyCfg.Split()
		if err != nil {
			t.Fatalf("Split: %v", err)
		}
		p, err := agent.Prepare(buildBaseRequest(baseRequestInput{TrustProfile: string(trust), ExecProfileName: e.Name}), agent.Runtime{}, agent.Content{}, agent.RecordInputs{})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if p.ConfigSHA256() == legacySpec.ConfigSHA256() {
			t.Fatalf("%s: ConfigSHA256 did not move; a correction is missing from the canonical policy", pname)
		}
		return legacySpec.Policy(), p.Spec().Policy()
	}

	t.Run("memory", func(t *testing.T) {
		e, _ := profile.ExecByName("coding-v2")
		if !e.Memory {
			t.Skip("coding-v2 no longer declares Memory=true; correction moot")
		}
		legacy, native := prepare(t, "coding-v2")
		if legacy.Memory {
			t.Fatal("legacy recorded memory=true; the drop defect is gone (Requested now round-trips Memory)")
		}
		if !native.Memory {
			t.Fatal("Prepare failed to record the profile's memory=true")
		}
	})

	t.Run("worktree", func(t *testing.T) {
		e, _ := profile.ExecByName("eval-swe-v1")
		if e.Worktree != "off" {
			t.Skipf("eval-swe-v1 no longer demands worktree=off (got %q); correction moot", e.Worktree)
		}
		legacy, native := prepare(t, "eval-swe-v1")
		if legacy.Worktree != "auto" {
			t.Fatalf("legacy worktree = %q, want the documented defect value %q", legacy.Worktree, "auto")
		}
		if native.Worktree != "off" {
			t.Fatalf("Prepare worktree = %q, want the profile-demanded %q", native.Worktree, "off")
		}
	})
}
