package headless

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/runspec"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// correctedFields are the policy fields the legacy buildBaseConfig→Split path
// under-records because agent.Config cannot faithfully carry them, so the native
// buildBaseRequest→Prepare path corrects the RECORD (never behavior — every one
// is recording-only in headless, where tools are always injected and
// plan.ForceWorktree drives isolation):
//   - memory: Config carries only the memory.Store binding, never a policy bool,
//     so legacy always records false though coding-v2/interactive-v2 declare
//     Memory=true. (Always differs.)
//   - worktree: Requested() withholds TrustProfile AND the exec profile, so
//     legacy records the trusted-local floor "auto", dropping any exec-profile
//     demand (eval-swe-v1 DEMANDS off). (Differs per profile/trust.)
//   - read_window / read_outline: buildBaseConfig never puts them in the policy
//     (headless builds the read tools from the flag directly, injected as a
//     Runtime binding — loop_shared only falls back to the policy value when
//     rt.Tools is nil, which headless never leaves), so legacy records the
//     profile default while native records the flag the operator set. (Differs
//     only when the flag is explicitly set.)
//
// All move ConfigSHA256. Same defect class as agent's
// TestPrepareCorrectsWorktreeFloorUnderContainerTrust, broadened.
var correctedFields = map[string]bool{"memory": true, "worktree": true, "read_window": true, "read_outline": true}

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
		{"read-window", func() profile.Overrides {
			w, o := 200, true
			return profile.Overrides{ReadWindow: &w, ReadOutline: &o}
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
	prepare := func(t *testing.T, pname string, ov profile.Overrides) (legacy, native runspec.PolicyValue) {
		t.Helper()
		e, err := profile.ExecByName(pname)
		if err != nil {
			t.Fatal(err)
		}
		posture := profile.TrustPosture{Selected: trust, Posture: profile.FloorFor(trust), Surface: "headless"}
		resolved, _, err := profile.Resolve(posture, e, ov)
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
		p, err := agent.Prepare(buildBaseRequest(baseRequestInput{TrustProfile: string(trust), ExecProfileName: e.Name, Overrides: ov}), agent.Runtime{}, agent.Content{}, agent.RecordInputs{})
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
		legacy, native := prepare(t, "coding-v2", profile.Overrides{})
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
		legacy, native := prepare(t, "eval-swe-v1", profile.Overrides{})
		if legacy.Worktree != "auto" {
			t.Fatalf("legacy worktree = %q, want the documented defect value %q", legacy.Worktree, "auto")
		}
		if native.Worktree != "off" {
			t.Fatalf("Prepare worktree = %q, want the profile-demanded %q", native.Worktree, "off")
		}
	})

	// read_window/read_outline are allowlisted out of the equivalence oracle, so
	// without this directional pin a builder bug that drops (or swaps) the
	// forwarding would only ever produce diffs on allowlisted keys and no test
	// would fail: legacy never carries them in the policy (buildBaseConfig has no
	// field), native must record the operator's explicit flag values.
	t.Run("read-window", func(t *testing.T) {
		w, o := 200, true
		legacy, native := prepare(t, "coding-v2", profile.Overrides{ReadWindow: &w, ReadOutline: &o})
		if legacy.ReadWindow == w {
			t.Fatalf("legacy recorded the explicit read window %d; the drop defect is gone — retire the correction", w)
		}
		if native.ReadWindow != w {
			t.Fatalf("Prepare read_window = %d, want the explicit flag value %d", native.ReadWindow, w)
		}
		if native.ReadOutline != o {
			t.Fatalf("Prepare read_outline = %v, want the explicit flag value %v", native.ReadOutline, o)
		}
	})
}

// The trust plan's MinIsolation can exceed the trust floor (untrusted raises
// FloorFor's process to kernel — headless/trust.go). The legacy path forwarded
// the raised value via Config.Requested(); the native builder must too, or the
// loop's fail-closed sandbox gate and the recorded min_isolation silently fall
// back to the weaker floor. The equivalence matrix can't see this (its trusts
// have no plan-raised isolation), so it is pinned directly.
func TestBaseRequestForwardsPlanMinIsolation(t *testing.T) {
	req := buildBaseRequest(baseRequestInput{
		TrustProfile: string(profile.Untrusted), ExecProfileName: "coding-v2",
		MinIsolation: sandbox.IsolationKernel,
	})
	p, err := agent.Prepare(req, agent.Runtime{}, agent.Content{}, agent.RecordInputs{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := p.Spec().Policy().MinIsolation; got != sandbox.IsolationKernel {
		t.Fatalf("min_isolation = %v, want the plan-raised %v (floor for untrusted is only %v)",
			got, sandbox.IsolationKernel, profile.FloorFor(profile.Untrusted).MinIsolation)
	}
}
