// Package docker is the container-backed sandbox backend. It runs the agent's
// Exec inside a container (the IsolationProcess tier — a shared host kernel) and,
// by flipping one runtime flag to gVisor's runsc, the IsolationKernel tier — a
// userspace application kernel. File ops (ReadFile/WriteFile/ListDir + the path
// fence) are delegated to an embedded *local.Sandbox operating host-side on the
// workspace dir, which is bind-mounted into the container at /workspace so the
// host write and the in-container read see the same bytes (same inode via the
// mount). See ../../SANDBOX.md for the decided direction and ../sandbox.go for the
// interface this implements.
//
// The threat model is narrow and deliberate: the EXECUTED command is hostile.
// File reads/writes/listing are OUR Go code (already fenced) on the workspace —
// not part of the threat surface. So only Exec is containerized; everything else
// rides the tested local fence.
package docker

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Options configures a docker Sandbox. The zero value is unusable on its own —
// New fills in defaults via withDefaults — but every field is documented with the
// specific escape or failure mode it governs so the security posture is legible
// at the call site, not buried in New.
type Options struct {
	// Image is the container image to run. It MUST contain everything the toolset
	// shells out to — sh (the run tool), rg (the search tool execs it directly),
	// git, and go if in-sandbox builds are expected. See ./Dockerfile. Empty =>
	// DefaultImage.
	Image string

	// Runtime selects the OCI runtime. "" (or "runc") is the standard
	// shared-kernel container => IsolationProcess. "runsc" is gVisor — syscalls
	// hit a userspace app-kernel, not the host kernel => IsolationKernel. This is
	// the one-flag gVisor graduation (SANDBOX.md): it threads straight into
	// `docker run --runtime=`. The host must have the runtime registered with the
	// daemon; we don't verify that here — a missing runtime surfaces as a New
	// error from the daemon.
	Runtime string

	// Network, when false (the default), runs the container with --network none:
	// no exfiltration, no callbacks, no module fetches. This is the point for
	// genuinely untrusted code — the workspace must be self-contained (see the
	// module story: ExtraMounts can carry a read-only GOMODCACHE). Set true for
	// trusted-but-isolated cases that need egress.
	Network bool

	// User is the in-container user as "uid:gid". Defaulting it to the HOST uid:gid
	// (New does this when empty) serves two purposes at once: it runs non-root in
	// the container, AND files the container writes to the bind mount are owned by
	// the host user, so host-side file ops can read them back (the uid-mismatch
	// gotcha, §13). Set explicitly to override.
	User string

	// WorkdirMount is the in-container path the workspace dir is bind-mounted to
	// and the working directory for every Exec. Empty => DefaultWorkdir
	// ("/workspace"). The mount is the ONLY writable mount; the root filesystem is
	// read-only.
	WorkdirMount string

	// Memory, PidsLimit, CPUs cap resource use to contain a fork bomb, a memory
	// bomb, or CPU spin (§6). They map to --memory, --pids-limit, --cpus. Empty/0
	// => the DefaultLimits below. A limit of "0" / NoLimit can be set explicitly to
	// disable a given cap (not recommended for untrusted code).
	Memory    string // e.g. "512m"; "" => DefaultMemory.
	PidsLimit int    // e.g. 256; 0 => DefaultPidsLimit; negative => unlimited.
	CPUs      string // e.g. "1.0"; "" => DefaultCPUs.

	// WritableRootFS drops the --read-only root filesystem. The default (false,
	// read-only) is the right posture for OUR sandbox image, where the workspace
	// mount is the only thing a task should write. Set true ONLY for prebuilt
	// third-party images whose tooling must write outside the workspace — e.g.
	// SWE-bench instance images, where the conda env under /opt/miniconda3 needs
	// .pyc/install writes for the baked-in test tooling to run. This widens the
	// blast radius from "workspace only" to "container filesystem" — the container
	// boundary (caps dropped, no-new-privileges, resource caps, --rm teardown) is
	// then the whole guarantee, and the rootfs is no longer attestable as pristine.
	WritableRootFS bool

	// ExtraMounts are additional bind mounts as raw `docker run -v` specs, e.g.
	// "/home/u/go/pkg/mod:/go/pkg/mod:ro" to give in-container `go build` a
	// read-only host module cache while --network none stays on (the module story,
	// §13). Each is passed through verbatim; the caller owns the trust decision for
	// what it mounts (mount read-only for anything outside the workspace).
	ExtraMounts []string

	// ExtraEnv are environment variables set on every Exec, each "KEY=VALUE". Use
	// for things like GOFLAGS=-mod=mod or GOPROXY=off that a network-off build
	// needs. Per-command Env (sandbox.Command.Env) is merged on top of these.
	ExtraEnv []string

	// Binary is the container CLI to invoke. Defaults to "docker"; Podman is
	// CLI-compatible, so setting "podman" works without any other change. We add no
	// Podman-specific code — this is the entire Podman story (a configurable
	// binary), per the design decision.
	Binary string

	// StartTimeout bounds `docker run` (image pull + container start). 0 =>
	// DefaultStartTimeout. A slow pull shouldn't hang New forever.
	StartTimeout time.Duration

	// MaxOutputBytes caps how much stdout (and, separately, stderr) Exec retains
	// from a command. Hostile code can flood output (`yes`, a stderr loop) to OOM
	// the agent; the runner stops STORING past this many bytes while still draining
	// the stream so the process isn't blocked. 0 => DefaultMaxOutputBytes; negative
	// => unlimited (only for trusted use). The tool layer clips again for the model
	// (agent/tools.go); this is the memory-safety bound underneath it (P4).
	MaxOutputBytes int64
}

// Defaults. Tuned for "run one untrusted CPU command safely", not for a
// long-lived service. Override per Options field when a task needs more.
const (
	DefaultImage     = "driver-os-sandbox:latest"
	DefaultWorkdir   = "/workspace"
	DefaultMemory    = "512m"
	DefaultPidsLimit = 256
	DefaultCPUs      = "1.0"
	DefaultBinary    = "docker"

	// DefaultMaxOutputBytes caps retained stdout/stderr per Exec (8 MiB each) so a
	// flooding command can't OOM the agent.
	DefaultMaxOutputBytes = 8 << 20

	// containerLabel tags every container this backend starts, so leaked
	// containers (a SIGKILL/crash bypasses Close) are findable and reapable:
	// `docker ps -aq --filter label=driver-os-sandbox`.
	containerLabel = "driver-os-sandbox"

	// DefaultStartTimeout bounds container startup (NOT command execution — that's
	// sandbox.Command.Timeout). Generous because a first-run image pull can be slow.
	DefaultStartTimeout = 2 * time.Minute

	// runscRuntime is gVisor's OCI runtime name; Runtime=="runsc" is the
	// IsolationKernel graduation.
	runscRuntime = "runsc"
)

// withDefaults returns a copy of o with every empty/zero field filled from the
// Default* constants. Done once in New so the rest of the backend reads concrete
// values and the defaults live in exactly one place.
func (o Options) withDefaults() Options {
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.WorkdirMount == "" {
		o.WorkdirMount = DefaultWorkdir
	}
	if o.Memory == "" {
		o.Memory = DefaultMemory
	}
	if o.PidsLimit == 0 {
		o.PidsLimit = DefaultPidsLimit
	}
	if o.CPUs == "" {
		o.CPUs = DefaultCPUs
	}
	if o.Binary == "" {
		o.Binary = DefaultBinary
	}
	if o.StartTimeout == 0 {
		o.StartTimeout = DefaultStartTimeout
	}
	if o.MaxOutputBytes == 0 {
		o.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return o
}

// isolation reports the isolation level these options yield: IsolationKernel for
// gVisor (runsc), IsolationProcess for a standard container. This is what
// Capabilities() advertises and what the MinIsolation policy gate checks against.
func (o Options) isolation() sandbox.Isolation {
	if o.Runtime == runscRuntime {
		return sandbox.IsolationKernel
	}
	return sandbox.IsolationProcess
}

// validate catches the misconfigurations we can detect before touching the
// daemon, so New fails with a clear message instead of an opaque docker error.
func (o Options) validate() error {
	if o.User != "" && !looksLikeUserSpec(o.User) {
		return fmt.Errorf("docker: User %q must be \"uid:gid\" (e.g. \"1000:1000\")", o.User)
	}
	// WorkdirMount is used both as the bind-mount target and as `docker exec -w`,
	// and is concatenated into in-container paths — so it must be a clean ABSOLUTE
	// container path, never "/" (which would shadow the whole rootfs) and never
	// contain ':' (which would misparse the --mount spec).
	if w := o.WorkdirMount; w != "" {
		if !strings.HasPrefix(w, "/") || w == "/" || strings.Contains(w, ":") || path.Clean(w) != w {
			return fmt.Errorf("docker: WorkdirMount %q must be a clean absolute path other than \"/\" with no ':'", w)
		}
	}
	return nil
}

// looksLikeUserSpec is a cheap sanity check that s is a "uid:gid" pair of
// non-empty numeric fields — enough to catch a fat-fingered User option before
// it reaches the daemon, without reimplementing full validation.
func looksLikeUserSpec(s string) bool {
	uid, gid, ok := strings.Cut(s, ":")
	if !ok || uid == "" || gid == "" {
		return false
	}
	return isAllDigits(uid) && isAllDigits(gid)
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
