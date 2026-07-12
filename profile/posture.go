package profile

import (
	"errors"
	"fmt"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Trust is one of the canonical operator-selected trust profiles from
// docs/specs/PROFILES.md §1. Names are intentionally not aliases.
type Trust string

const (
	// TrustedLocal is for code the operator wrote and trusts on the host.
	TrustedLocal Trust = "trusted-local"
	// ReviewedLocal is host-visible code execution subject to approval.
	ReviewedLocal Trust = "reviewed-local"
	// Container is for code that needs a process-isolated container boundary.
	Container Trust = "container"
	// Untrusted is for hostile code requiring the strictest shipped floor.
	Untrusted Trust = "untrusted"
)

// ErrMissingTrust explains the threat-model choice required before S2b may run
// a headless command. It enumerates choices rather than recommending a default.
var ErrMissingTrust = errors.New("trust profile is required; select for the code's threat model:\n" +
	"  trusted-local: code you wrote and trust; runs on the host.\n" +
	"  reviewed-local: host-visible code; every execution action needs approval.\n" +
	"  container: code needing a container process boundary.\n" +
	"  untrusted: hostile code; containerized with no secrets and no network")

// ParseTrust accepts only the four verbatim canonical names in
// docs/specs/PROFILES.md §1.
func ParseTrust(s string) (Trust, error) {
	t := Trust(s)
	switch t {
	case TrustedLocal, ReviewedLocal, Container, Untrusted:
		return t, nil
	default:
		return "", fmt.Errorf("unknown trust profile %q; expected trusted-local, reviewed-local, container, or untrusted", s)
	}
}

// SandboxClass is the sandbox boundary class, ordered weakest to strongest.
type SandboxClass int

const (
	// SandboxLocal is host-local execution; command gating is an approval mode,
	// not a sandbox class.
	SandboxLocal SandboxClass = iota
	// SandboxContainer is a container execution boundary.
	SandboxContainer
)

// Satisfies reports whether c meets the sandbox floor.
func (c SandboxClass) Satisfies(floor SandboxClass) bool { return c >= floor }

func (c SandboxClass) String() string {
	switch c {
	case SandboxLocal:
		return "local"
	case SandboxContainer:
		return "container"
	default:
		return fmt.Sprintf("sandbox-class(%d)", c)
	}
}

// Worktree controls whether execution must use a worktree, ordered weakest to
// strongest as required by docs/specs/PROFILES.md §1.
type Worktree int

const (
	// WorktreeAuto permits today's automatic worktree behavior.
	WorktreeAuto Worktree = iota
	// WorktreeRequired requires an isolated worktree.
	WorktreeRequired
)

// Satisfies reports whether w meets the worktree floor.
func (w Worktree) Satisfies(floor Worktree) bool { return w >= floor }

func (w Worktree) String() string {
	if w == WorktreeAuto {
		return "auto"
	}
	if w == WorktreeRequired {
		return "required"
	}
	return fmt.Sprintf("worktree(%d)", w)
}

// Approval is a partial order. Interactive and policy-gated approval are
// incomparable; classifier-gated approval does not satisfy the generic
// human-review requirement; deny-all is strongest because it never bypasses
// approval.
type Approval int

const (
	// ApprovalNone permits execution without an approval mechanism.
	ApprovalNone Approval = iota
	// ApprovalRequired is a floor-only generic requirement represented on a
	// canonical floor. It is satisfied by itself, interactive or policy-gated
	// approval, but not by no approval or classifier-gated approval.
	ApprovalRequired
	// ApprovalInteractive requires a person to approve actions.
	ApprovalInteractive
	// ApprovalPolicyGated requires a default-deny approval policy.
	ApprovalPolicyGated
	// ApprovalDenyAll rejects every action and therefore satisfies every floor.
	ApprovalDenyAll
	// ApprovalClassifierGated routes commands through a probabilistic
	// classifier. It is not a human-review mechanism or a security boundary.
	ApprovalClassifierGated
)

// Satisfies reports whether a meets floor under Approval's partial order.
func (a Approval) Satisfies(floor Approval) bool {
	if a == ApprovalDenyAll || floor == ApprovalNone {
		return true
	}
	switch floor {
	case ApprovalRequired:
		return a == ApprovalRequired || a == ApprovalInteractive || a == ApprovalPolicyGated
	case ApprovalInteractive, ApprovalPolicyGated:
		return a == floor
	case ApprovalDenyAll:
		return false
	default:
		return false
	}
}

func (a Approval) String() string {
	switch a {
	case ApprovalNone:
		return "none"
	case ApprovalRequired:
		return "required"
	case ApprovalInteractive:
		return "interactive"
	case ApprovalPolicyGated:
		return "policy-gated"
	case ApprovalDenyAll:
		return "deny-all"
	case ApprovalClassifierGated:
		return "classifier-gated"
	default:
		return fmt.Sprintf("approval(%d)", a)
	}
}

// Secrets is ordered from ambient host secrets to no secret exposure.
type Secrets int

const (
	// SecretsAmbient exposes the ambient environment.
	SecretsAmbient Secrets = iota
	// SecretsAllowlist exposes only a clean-environment allowlist.
	SecretsAllowlist
	// SecretsNone exposes no secrets.
	SecretsNone
)

// Satisfies reports whether s meets the secret-exposure floor.
func (s Secrets) Satisfies(floor Secrets) bool { return s >= floor }

func (s Secrets) String() string {
	switch s {
	case SecretsAmbient:
		return "ambient"
	case SecretsAllowlist:
		return "allowlist"
	case SecretsNone:
		return "none"
	}
	return fmt.Sprintf("secrets(%d)", s)
}

// Network is ordered from unrestricted connectivity to network-off.
type Network int

const (
	// NetworkUnrestricted permits network connectivity.
	NetworkUnrestricted Network = iota
	// NetworkAllowlisted permits only allowlisted destinations.
	NetworkAllowlisted
	// NetworkOff disables network connectivity.
	NetworkOff
)

// Satisfies reports whether n meets the network floor.
func (n Network) Satisfies(floor Network) bool { return n >= floor }

func (n Network) String() string {
	switch n {
	case NetworkUnrestricted:
		return "unrestricted"
	case NetworkAllowlisted:
		return "allowlisted"
	case NetworkOff:
		return "off"
	}
	return fmt.Sprintf("network(%d)", n)
}

// Posture is the six-dimensional trust floor specified by
// docs/specs/PROFILES.md §1. Its tags are the completeness anchor for the
// descriptor table required by §3.
type Posture struct {
	Sandbox      SandboxClass      `trust:"sandbox"`
	MinIsolation sandbox.Isolation `trust:"min-isolation"`
	Worktree     Worktree          `trust:"worktree"`
	Approval     Approval          `trust:"approval"`
	Secrets      Secrets           `trust:"secrets"`
	Network      Network           `trust:"network"`
}

// RequiresNetworkOff reports whether this posture requires a sandbox with no
// network connectivity. It deliberately uses the Network partial order so a
// future floor stronger than NetworkOff inherits the requirement.
func (p Posture) RequiresNetworkOff() bool { return p.Network.Satisfies(NetworkOff) }

// FloorFor returns the complete non-weakening posture floor for t.
func FloorFor(t Trust) Posture {
	switch t {
	case TrustedLocal:
		return Posture{SandboxLocal, sandbox.IsolationNone, WorktreeAuto, ApprovalNone, SecretsAmbient, NetworkUnrestricted}
	case ReviewedLocal:
		// The worktree floor was amended to auto at S3 (2026-07-10): interactive
		// per-command approval substitutes for workspace isolation as the review
		// mechanism, and the TUI's in-place editing is this profile's primary
		// interactive use. HEADLESS reviewed-local still forces a worktree in its
		// plan; that tightening is permitted by this floor.
		return Posture{SandboxLocal, sandbox.IsolationNone, WorktreeAuto, ApprovalRequired, SecretsAllowlist, NetworkUnrestricted}
	case Container:
		// Command approval inside a container adds friction without a security
		// boundary — containment is the control; untrusted keeps policy gating
		// on top of containment (decided 2026-07-10, S2b).
		return Posture{SandboxContainer, sandbox.IsolationProcess, WorktreeRequired, ApprovalNone, SecretsAllowlist, NetworkOff}
	case Untrusted:
		return Posture{SandboxContainer, sandbox.IsolationProcess, WorktreeRequired, ApprovalPolicyGated, SecretsNone, NetworkOff}
	default:
		return Posture{}
	}
}
