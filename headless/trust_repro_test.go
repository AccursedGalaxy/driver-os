package headless

import (
	"errors"
	"flag"
	"reflect"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Oracle for PROFILES.md S2b activation in cmd/agent: -trust is mandatory
// headless (fail-closed with the enumerating error), and planTrust turns a
// trust profile + the explicitly-set flags into an assembled posture plan —
// floors LIFT unset defaults, explicitly-set weaker flags are hard errors
// naming the dimension.

func parseTrustFlags(t *testing.T, args ...string) (*agentFlags, map[string]bool) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := registerFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	explicit := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })
	return f, explicit
}

func TestResolveTrustFailClosed(t *testing.T) {
	if _, err := resolveTrust(""); !errors.Is(err, profile.ErrMissingTrust) {
		t.Fatalf("empty -trust must fail closed with profile.ErrMissingTrust; got %v", err)
	}
	tr, err := resolveTrust("untrusted")
	if err != nil || tr != profile.Untrusted {
		t.Fatalf("resolveTrust(untrusted) = %v, %v", tr, err)
	}
	if _, err := resolveTrust("TRUSTED-LOCAL"); err == nil {
		t.Fatal("aliases/case-folding must be rejected")
	}
}

func TestPlanTrustTrustedLocalIsTodaysPosture(t *testing.T) {
	f, explicit := parseTrustFlags(t)
	plan, err := planTrust(profile.TrustedLocal, explicit, f)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SandboxKind != "local" || plan.Network || plan.ForceWorktree || plan.GatePolicy != nil || plan.EnvAllowlist != nil {
		t.Fatalf("trusted-local must be today's posture unchanged: %+v", plan)
	}
}

func TestPlanTrustContainerLiftsDefaults(t *testing.T) {
	f, explicit := parseTrustFlags(t)
	plan, err := planTrust(profile.Container, explicit, f)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SandboxKind != "docker" {
		t.Fatalf("container must lift the default sandbox to docker: %+v", plan)
	}
	if plan.Network {
		t.Fatalf("container network is OFF (decided 2026-07-10): %+v", plan)
	}
	if !plan.ForceWorktree {
		t.Fatalf("container requires a worktree: %+v", plan)
	}
	if plan.MinIsolation < sandbox.IsolationProcess {
		t.Fatalf("container must require >=process isolation: %+v", plan)
	}
	if plan.GatePolicy != nil {
		t.Fatalf("containment is the control for container: no command gate: %+v", plan)
	}
}

func TestPostureForPlanMatchesAssembledRuntime(t *testing.T) {
	cases := []struct {
		trust profile.Trust
		want  profile.Posture
	}{
		{profile.TrustedLocal, profile.Posture{Sandbox: profile.SandboxLocal, MinIsolation: sandbox.IsolationNone, Worktree: profile.WorktreeAuto, Approval: profile.ApprovalNone, Secrets: profile.SecretsAmbient, Network: profile.NetworkUnrestricted}},
		{profile.ReviewedLocal, profile.Posture{Sandbox: profile.SandboxLocal, MinIsolation: sandbox.IsolationNone, Worktree: profile.WorktreeRequired, Approval: profile.ApprovalPolicyGated, Secrets: profile.SecretsAllowlist, Network: profile.NetworkUnrestricted}},
		{profile.Container, profile.Posture{Sandbox: profile.SandboxContainer, MinIsolation: sandbox.IsolationProcess, Worktree: profile.WorktreeRequired, Approval: profile.ApprovalNone, Secrets: profile.SecretsAllowlist, Network: profile.NetworkOff}},
		{profile.Untrusted, profile.Posture{Sandbox: profile.SandboxContainer, MinIsolation: sandbox.IsolationKernel, Worktree: profile.WorktreeRequired, Approval: profile.ApprovalPolicyGated, Secrets: profile.SecretsNone, Network: profile.NetworkOff}},
	}
	for _, tc := range cases {
		f, explicit := parseTrustFlags(t)
		plan, err := planTrust(tc.trust, explicit, f)
		if err != nil {
			t.Fatalf("planTrust(%s): %v", tc.trust, err)
		}
		got, err := postureForPlan(plan)
		if err != nil {
			t.Fatalf("postureForPlan(%s): %v", tc.trust, err)
		}
		if got != tc.want {
			t.Errorf("postureForPlan(%s) = %+v, want %+v", tc.trust, got, tc.want)
		}
	}
}

func TestPlanTrustExplicitWeakeningIsAnError(t *testing.T) {
	cases := []struct {
		trust     profile.Trust
		args      []string
		dimension string
	}{
		{profile.Container, []string{"-sandbox", "local"}, "sandbox"},
		{profile.Container, []string{"-network"}, "network"},
		{profile.Untrusted, []string{"-network=true"}, "network"},
		{profile.ReviewedLocal, []string{"-worktree=false"}, "worktree"},
	}
	for _, c := range cases {
		f, explicit := parseTrustFlags(t, c.args...)
		_, err := planTrust(c.trust, explicit, f)
		if err == nil {
			t.Fatalf("%s + %v: explicit weakening accepted", c.trust, c.args)
		}
		if !strings.Contains(err.Error(), c.dimension) {
			t.Fatalf("%s + %v: error must name dimension %q; got: %v", c.trust, c.args, c.dimension, err)
		}
	}
}

func TestPlanTrustReviewedLocalHeadless(t *testing.T) {
	f, explicit := parseTrustFlags(t)
	plan, err := planTrust(profile.ReviewedLocal, explicit, f)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SandboxKind != "local" || !plan.ForceWorktree {
		t.Fatalf("reviewed-local is gated LOCAL execution with a required worktree: %+v", plan)
	}
	if plan.GatePolicy == nil || plan.GatePolicy.Name != profile.DefaultHeadlessPolicy || plan.GatePolicy.Hash() == "" {
		t.Fatalf("headless reviewed-local must carry the shipped versioned default-deny policy: %+v", plan.GatePolicy)
	}
	if len(plan.EnvAllowlist) == 0 {
		t.Fatalf("reviewed-local must run on a clean-env allowlist: %+v", plan)
	}
	has := func(name string) bool {
		for _, v := range plan.EnvAllowlist {
			if v == name {
				return true
			}
		}
		return false
	}
	if !has("PATH") {
		t.Fatalf("clean-env allowlist must keep PATH (commands must still run): %v", plan.EnvAllowlist)
	}
	for _, secret := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if has(secret) {
			t.Fatalf("clean-env allowlist must not include provider keys: %v", plan.EnvAllowlist)
		}
	}
}

func TestPlanTrustUntrustedIsStrictest(t *testing.T) {
	f, explicit := parseTrustFlags(t)
	plan, err := planTrust(profile.Untrusted, explicit, f)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SandboxKind != "docker" || plan.Network || !plan.ForceWorktree {
		t.Fatalf("untrusted must be docker, network-off, worktree-required: %+v", plan)
	}
	if plan.GatePolicy == nil {
		t.Fatalf("untrusted keeps default-deny command gating on top of containment: %+v", plan)
	}
	if plan.MinIsolation < sandbox.IsolationProcess {
		t.Fatalf("untrusted must require >=process isolation: %+v", plan)
	}
}
func TestPlanTrustLegacyUntrustedNormalizesToSamePosture(t *testing.T) {
	profileFlags, profileExplicit := parseTrustFlags(t)
	fromProfile, err := planTrust(profile.Untrusted, profileExplicit, profileFlags)
	if err != nil {
		t.Fatal(err)
	}
	legacyFlags, legacyExplicit := parseTrustFlags(t, "-untrusted")
	fromLegacy, err := planTrust(profile.TrustedLocal, legacyExplicit, legacyFlags)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromProfile, fromLegacy) {
		t.Fatalf("legacy -untrusted must produce the same normalized posture as -trust untrusted:\nprofile: %+v\nlegacy:  %+v", fromProfile, fromLegacy)
	}
	if fromProfile.AllowProjectSkills {
		t.Fatal("untrusted normalized plan must suppress project-controlled skills")
	}
	if fromProfile.Runtime != "runsc" || fromProfile.MinIsolation < sandbox.IsolationKernel {
		t.Fatalf("untrusted normalized plan must require kernel isolation: %+v", fromProfile)
	}
}

func TestBestOfAttemptPreservesNormalizedTrustPlan(t *testing.T) {
	f, explicit := parseTrustFlags(t)
	plan, err := planTrust(profile.Untrusted, explicit, f)
	if err != nil {
		t.Fatal(err)
	}
	opts := bestOfCLIOptions{TrustPlan: plan, Reviewer: &scriptedBestOfReviewer{}}
	attempt := bestOfAttemptOptions(opts)
	if !reflect.DeepEqual(attempt.TrustPlan, plan) {
		t.Fatalf("best-of attempt changed normalized trust plan: got %+v, want %+v", attempt.TrustPlan, plan)
	}
	if attempt.Reviewer != nil {
		t.Fatal("best-of attempt must still strip the winner-only reviewer")
	}
}

func TestPlanTrustUntrustedRejectsExplicitWeakerLegacyInputs(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		field string
	}{
		{[]string{"-untrusted=false"}, "untrusted"},
		{[]string{"-runtime=runc"}, "runtime"},
	} {
		f, explicit := parseTrustFlags(t, tc.args...)
		_, err := planTrust(profile.Untrusted, explicit, f)
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("%v: want field-specific %s error, got %v", tc.args, tc.field, err)
		}
	}
}
