package headless

import (
	"fmt"

	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/gated"
)

// resolveTrust requires an explicit, canonical trust profile for headless runs.
func resolveTrust(raw string) (profile.Trust, error) {
	if raw == "" {
		return "", fmt.Errorf("-trust: %w", profile.ErrMissingTrust)
	}
	return profile.ParseTrust(raw)
}

type trustPlan struct {
	Trust              profile.Trust
	SandboxKind        string
	Runtime            string
	Network            bool
	ForceWorktree      bool
	MinIsolation       sandbox.Isolation
	GatePolicy         *profile.ApprovalPolicy
	EnvAllowlist       []string
	Secrets            profile.Secrets
	AllowProjectSkills bool
}

// planTrust normalizes the selected profile and legacy safety flags into the
// only safety posture consumed by runtime setup. Legacy -untrusted may tighten
// any selected profile; explicit settings that weaken the resulting floor are
// rejected with the affected field named.
func planTrust(t profile.Trust, explicit map[string]bool, f *agentFlags) (trustPlan, error) {
	if explicit["untrusted"] {
		if *f.untrusted {
			t = profile.Untrusted
		} else if t == profile.Untrusted {
			return trustPlan{}, fmt.Errorf("trust %q forbids weakening untrusted mode", t)
		}
	}
	p := trustPlan{
		Trust: t, SandboxKind: *f.sandboxKind, Runtime: *f.runtime, Network: *f.network,
		Secrets: profile.SecretsAmbient, AllowProjectSkills: true,
	}
	switch t {
	case profile.TrustedLocal:
		// Today's posture is intentional: nothing is forced.
	case profile.ReviewedLocal:
		if explicit["worktree"] && f.worktree.set && !f.worktree.val {
			return trustPlan{}, fmt.Errorf("trust %q forbids weakening worktree requirement", t)
		}
		p.ForceWorktree = true
		policy, err := profile.PolicyByName(profile.DefaultHeadlessPolicy)
		if err != nil {
			return trustPlan{}, err
		}
		p.GatePolicy = &policy
		p.EnvAllowlist = profile.CleanEnvAllowlist()
		p.Secrets = profile.SecretsAllowlist
	case profile.Container, profile.Untrusted:
		if explicit["sandbox"] && *f.sandboxKind == "local" {
			return trustPlan{}, fmt.Errorf("trust %q forbids weakening sandbox to local", t)
		}
		if explicit["network"] && *f.network {
			return trustPlan{}, fmt.Errorf("trust %q forbids weakening network policy", t)
		}
		if explicit["worktree"] && f.worktree.set && !f.worktree.val {
			return trustPlan{}, fmt.Errorf("trust %q forbids weakening worktree requirement", t)
		}
		p.SandboxKind = "docker"
		p.Network = false
		p.ForceWorktree = true
		p.MinIsolation = sandbox.IsolationProcess
		p.Secrets = profile.SecretsAllowlist
		if t == profile.Untrusted {
			if explicit["runtime"] && *f.runtime != "runsc" {
				return trustPlan{}, fmt.Errorf("trust %q forbids weakening runtime below runsc", t)
			}
			p.Runtime = "runsc"
			p.MinIsolation = sandbox.IsolationKernel
			p.Secrets = profile.SecretsNone
			p.AllowProjectSkills = false
		}
		if t == profile.Untrusted {
			policy, err := profile.PolicyByName(profile.DefaultHeadlessPolicy)
			if err != nil {
				return trustPlan{}, err
			}
			p.GatePolicy = &policy
		}
	default:
		return trustPlan{}, fmt.Errorf("unknown trust profile %q", t)
	}

	posture, err := postureForPlan(p)
	if err != nil {
		return trustPlan{}, err
	}
	if err := profile.Enforce(t, posture); err != nil {
		return trustPlan{}, err
	}
	return p, nil
}

func postureForPlan(p trustPlan) (profile.Posture, error) {
	var sandboxClass profile.SandboxClass
	switch p.SandboxKind {
	case "local":
		sandboxClass = profile.SandboxLocal
	case "docker":
		sandboxClass = profile.SandboxContainer
	default:
		return profile.Posture{}, fmt.Errorf("unknown sandbox %q", p.SandboxKind)
	}
	worktree := profile.WorktreeAuto
	if p.ForceWorktree {
		worktree = profile.WorktreeRequired
	}
	approval := profile.ApprovalNone
	if p.GatePolicy != nil {
		approval = profile.ApprovalPolicyGated
	}
	network := profile.NetworkUnrestricted
	if p.SandboxKind == "docker" && !p.Network {
		network = profile.NetworkOff
	}
	return profile.Posture{Sandbox: sandboxClass, MinIsolation: p.MinIsolation, Worktree: worktree, Approval: approval, Secrets: p.Secrets, Network: network}, nil
}

func approvalPolicyAdapter(policy profile.ApprovalPolicy) gated.Policy {
	return func(cmd sandbox.Command) gated.Verdict {
		if cmd.Path == "sh" && len(cmd.Args) == 2 && cmd.Args[0] == "-c" {
			if policy.Allows(cmd.Args[1]) {
				return gated.Allow
			}
			return gated.Deny
		}
		if policy.AllowsExec(cmd.Path, cmd.Args) {
			return gated.Allow
		}
		return gated.Deny
	}
}
