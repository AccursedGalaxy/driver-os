package agent

import (
	"testing"

	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/runspec"
)

func ptr[T any](v T) *T { return &v }

// Prepare derives the recording identity from the resolution that actually ran.
// A caller cannot supply a provenance view of its own — the seam overwrites it.
func TestPrepareDerivesRecordMetaFromResolution(t *testing.T) {
	req := runspec.RequestedConfig{
		TrustProfile:    ptr("trusted-local"),
		ExecProfileName: ptr("coding-v2"),
	}
	// A caller trying to smuggle in its own provenance view.
	rt := Runtime{Record: RecordMeta{
		TrustProfile:    "container",
		ExecProfileName: "eval-swe-v1",
		Canonical:       false,
		FieldProvenance: map[string]string{"max_iters": "fabricated"},
	}}

	p, err := Prepare(req, rt, Content{Task: "t"}, RecordInputs{InvocationSurface: "driver-run"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	got := p.Runtime().Record

	if got.TrustProfile != "trusted-local" || got.ExecProfileName != "coding-v2" {
		t.Fatalf("identity not derived from resolution: trust=%q profile=%q", got.TrustProfile, got.ExecProfileName)
	}
	if got.InvocationSurface != "driver-run" {
		t.Fatalf("surface-specific input dropped: %q", got.InvocationSurface)
	}
	want, err := profile.ExecByName("coding-v2")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecProfileHash != want.Hash() {
		t.Fatalf("ExecProfileHash = %q, want %q", got.ExecProfileHash, want.Hash())
	}
	if !got.Canonical {
		t.Fatal("a profile-only run must be canonical")
	}
	if got.FieldProvenance["max_iters"] == "fabricated" {
		t.Fatal("caller-supplied FieldProvenance survived; Prepare must derive it from the trace")
	}
	if len(got.FieldProvenance) != len(profile.ProfileFields) {
		t.Fatalf("FieldProvenance covers %d fields, want %d", len(got.FieldProvenance), len(profile.ProfileFields))
	}
	// The trace Prepare keeps is the one that produced the spec.
	if p.Trace().TraceSHA256() != p.Spec().Trace().TraceSHA256() {
		t.Fatal("Prepared.Trace is not the spec's own resolution trace")
	}
}

// Without an exec profile there is nothing for profile-defaults to fill, so the
// legacy Config→Split path and the native Prepare path must resolve to exactly
// the same policy. This is the equivalence oracle S6d.7 deletes with Split().
//
// Scoped to the trusts whose floor the legacy path can actually represent — see
// TestPrepareCorrectsWorktreeFloorUnderContainerTrust for the two it cannot.
func TestPrepareEquivalentToSplitWithoutProfile(t *testing.T) {
	for _, trust := range []string{"trusted-local", "reviewed-local"} {
		for _, cli := range []struct {
			name string
			mut  func(*Config, *runspec.RequestedConfig)
		}{
			{"bare", func(*Config, *runspec.RequestedConfig) {}},
			{"max-iters", func(c *Config, r *runspec.RequestedConfig) {
				c.MaxIterations, r.MaxIterations = 7, ptr(7)
			}},
			{"verify-cmd", func(c *Config, r *runspec.RequestedConfig) {
				c.VerifyCmd, r.VerifyCmd = "go test ./...", ptr("go test ./...")
			}},
			{"review-rounds+fence", func(c *Config, r *runspec.RequestedConfig) {
				c.ReviewRounds, r.ReviewRounds = 3, ptr(3)
				c.TestFence, r.TestFence = []string{"*_test.go"}, ptr([]string{"*_test.go"})
			}},
		} {
			t.Run(trust+"/"+cli.name, func(t *testing.T) {
				// A legacy Config carries the floor values its flag layer already
				// resolved (headless/main.go:692) — reproduce that, or the
				// comparison is against a fixture no binary ever builds.
				tr, err := profile.ParseTrust(trust)
				if err != nil {
					t.Fatal(err)
				}
				floor := profile.FloorFor(tr)
				cfg := Config{
					TrustProfile:      trust,
					MinIsolation:      floor.MinIsolation,
					RequireNetworkOff: floor.RequiresNetworkOff(),
					Task:              "t",
				}
				req := runspec.RequestedConfig{TrustProfile: ptr(trust)}
				cli.mut(&cfg, &req)

				legacy, _, _, err := cfg.Split()
				if err != nil {
					t.Fatalf("Split: %v", err)
				}
				p, err := Prepare(req, Runtime{}, Content{Task: "t"}, RecordInputs{})
				if err != nil {
					t.Fatalf("Prepare: %v", err)
				}
				if p.ConfigSHA256() != legacy.ConfigSHA256() {
					t.Fatalf("ConfigSHA256 diverged\n  Split:   %s\n  Prepare: %s\n  policy(Split)=%s\n  policy(Prep)=%s",
						legacy.ConfigSHA256(), p.ConfigSHA256(), legacy.CanonicalPolicy(), p.Spec().CanonicalPolicy())
				}
			})
		}
	}
}

// A DEFECT THE SEAM CORRECTS (found by the equivalence oracle, S6d.1).
//
// Config.Requested() withholds TrustProfile from resolution, so the legacy path
// ALWAYS resolves the worktree floor as trusted-local's ("auto") — even for a
// container or untrusted run, whose floor demands "required". Today's
// ConfigSHA256 therefore MISSTATES the worktree floor on exactly the runs where
// the floor matters most (eval over public repos runs `container`).
//
// It is a recording defect, not an execution defect: PolicyValue.Worktree has no
// consumer in the loops — headless forces the worktree itself off its own
// profile resolution (plan.ForceWorktree, headless/main.go:407) — so the runs
// were isolated as promised; only the recorded policy lied about why.
//
// Prepare resolves trust, so the floor lands correctly and ConfigSHA256 MOVES
// for container/untrusted. That is a named, reviewed correction, not a
// regression: PROFILES.md §7.5's "ConfigSHA256 byte-identical" gate covers the
// values the legacy path represented FAITHFULLY, and this is the one it did not.
func TestPrepareCorrectsWorktreeFloorUnderContainerTrust(t *testing.T) {
	for _, trust := range []string{"container", "untrusted"} {
		t.Run(trust, func(t *testing.T) {
			tr, err := profile.ParseTrust(trust)
			if err != nil {
				t.Fatal(err)
			}
			if profile.FloorFor(tr).Worktree != profile.WorktreeRequired {
				t.Skipf("%s no longer demands a worktree; this correction is moot", trust)
			}

			legacy, _, _, err := Config{TrustProfile: trust, Task: "t"}.Split()
			if err != nil {
				t.Fatalf("Split: %v", err)
			}
			if got := legacy.Policy().Worktree; got != "auto" {
				t.Fatalf("legacy worktree = %q, want the documented defect value %q "+
					"(if this changed, Requested() now forwards trust and the correction below is already live)", got, "auto")
			}

			p, err := Prepare(runspec.RequestedConfig{TrustProfile: ptr(trust)}, Runtime{}, Content{Task: "t"}, RecordInputs{})
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if got := p.Spec().Policy().Worktree; got != "required" {
				t.Fatalf("Prepare worktree = %q, want %q — the trust floor must reach the recorded policy", got, "required")
			}
			if p.ConfigSHA256() == legacy.ConfigSHA256() {
				t.Fatal("ConfigSHA256 did not move; the corrected floor is not in the canonical policy")
			}
		})
	}
}

// PRESENCE CONTRACT (the reason Config is not a legal Prepare input).
//
// Config.Requested() derives presence from value != zero, so an explicitly-set
// -auto-verify=false is indistinguishable from an unset flag. Prepare forwards
// the exec profile to resolution, so feeding it a Config-shaped request would
// let coding-v2's AutoVerify=true be resurrected ON TOP of the operator's
// explicit false. A flag-built request (fs.Visit presence — what the flag
// layers already use for the profile-covered fields) carries the explicit false
// and it survives.
//
// This test is what keeps a future builder from "simplifying" back to
// Config.Requested() + a forwarded profile.
func TestPreparePresenceContractExplicitFalseSurvivesProfileDefault(t *testing.T) {
	coding, err := profile.ExecByName("coding-v2")
	if err != nil {
		t.Fatal(err)
	}
	if !coding.AutoVerify {
		t.Skip("coding-v2 no longer defaults AutoVerify=true; pick another field")
	}

	// (a) The sound path: presence carried explicitly, as a flag layer does.
	sound, err := Prepare(runspec.RequestedConfig{
		TrustProfile:    ptr("trusted-local"),
		ExecProfileName: ptr("coding-v2"),
		AutoVerify:      ptr(false), // operator said -auto-verify=false
	}, Runtime{}, Content{Task: "t"}, RecordInputs{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if sound.Spec().Verification().AutoVerify {
		t.Fatal("explicit -auto-verify=false was overridden by the profile default")
	}

	// (b) The unsound path this contract forbids: value-presence loses the false,
	// and the profile default is resurrected.
	cfg := Config{TrustProfile: "trusted-local", AutoVerify: false}
	req := cfg.Requested()
	req.ExecProfileName = ptr("coding-v2") // the "simplification" — forwarding a profile over Config presence
	unsound, err := Prepare(req, Runtime{}, Content{Task: "t"}, RecordInputs{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !unsound.Spec().Verification().AutoVerify {
		t.Fatal("expected the documented unsoundness (profile default resurrected over a zero-valued Config field); " +
			"if this no longer holds, Config.Requested() gained real presence and the contract comment must be updated")
	}
}
