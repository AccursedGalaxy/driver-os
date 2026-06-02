package docker

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// Sandbox is the container-backed sandbox.Sandbox. It composes the local backend
// (D1): file ops and the path fence are the embedded *local.Sandbox operating
// host-side on the workspace dir, while Exec — the ONLY part of the threat
// surface — is overridden to run inside a long-lived container (D3). Capabilities
// is also overridden to report the real isolation level instead of local's
// "none".
type Sandbox struct {
	*local.Sandbox // file ops + path fence, host-side on the workspace dir (D1).

	runner dockerRunner // how we talk to the container CLI (real or fake).
	opts   Options
	id     string // running container id, from `docker run -d`.
}

// compile-time proof we satisfy the interface. LimitedReader is inherited from
// the embedded *local.Sandbox — a bounded read of the workspace is host-side, so
// it needs no containerization.
var (
	_ sandbox.Sandbox       = (*Sandbox)(nil)
	_ sandbox.LimitedReader = (*Sandbox)(nil)
)

// New starts a long-lived container rooted at the workspace dir and returns a
// Sandbox that execs commands inside it. The container runs `sleep infinity` and
// stays up for the Sandbox's lifetime (D3); every Exec is a `docker exec` against
// it, and Close tears it down. dir is bind-mounted to opts.WorkdirMount
// (/workspace) read-write — the only writable mount — so a host-side WriteFile and
// an in-container read see the same bytes.
//
// Callers owning an UNTRUSTED task should root this at a throwaway copy, not the
// live checkout (§13) — the backend just takes dir; the trust decision is the
// caller's.
func New(ctx context.Context, dir string, opts Options) (*Sandbox, error) {
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}

	// The embedded local backend resolves dir to an absolute, cleaned path and is
	// the fence for all file ops. We reuse its resolved root for the bind mount so
	// the host path we hand Docker is exactly the fenced root.
	lsb, err := local.New(dir)
	if err != nil {
		return nil, err
	}

	if opts.User == "" {
		opts.User = currentUserSpec() // non-root + host-owned bind-mount writes (§6).
	}

	runner := dockerRunner(execRunner{binary: opts.Binary, maxOutput: opts.MaxOutputBytes})

	runCtx, cancel := context.WithTimeout(ctx, opts.StartTimeout)
	defer cancel()
	stdout, stderr, exit, err := runner.Run(runCtx, runArgs(opts, lsb.RealRoot()), nil)
	if err != nil {
		return nil, fmt.Errorf("docker: starting container: %w (is the %s daemon running?)", err, opts.Binary)
	}
	if exit != 0 {
		return nil, fmt.Errorf("docker: starting container: exit %d: %s", exit, strings.TrimSpace(string(stderr)))
	}
	id := strings.TrimSpace(string(stdout))
	if id == "" {
		return nil, fmt.Errorf("docker: starting container: empty container id from `run -d`")
	}

	sb := &Sandbox{Sandbox: lsb, runner: runner, opts: opts, id: id}

	// Fail CLOSED on the isolation guarantee: when gVisor was requested, VERIFY the
	// container actually runs under runsc before we let Capabilities() advertise
	// IsolationKernel — the policy gate trusts that claim to admit untrusted code,
	// so it must reflect observed container state, not just the option string. If
	// the daemon silently used a different runtime (or inspect fails), tear down
	// and refuse rather than overstate the boundary.
	if opts.Runtime == runscRuntime {
		if err := sb.verifyRuntime(runCtx, runscRuntime); err != nil {
			_ = sb.Close()
			return nil, err
		}
	}
	return sb, nil
}

// verifyRuntime confirms the running container's actual OCI runtime matches want,
// so a too-weak boundary can never masquerade as IsolationKernel (fail closed).
func (s *Sandbox) verifyRuntime(ctx context.Context, want string) error {
	stdout, stderr, exit, err := s.runner.Run(ctx, []string{"inspect", "-f", "{{.HostConfig.Runtime}}", s.id}, nil)
	if err != nil {
		return fmt.Errorf("docker: cannot verify container runtime is %q: %w", want, err)
	}
	if exit != 0 {
		return fmt.Errorf("docker: cannot verify container runtime is %q: %s", want, strings.TrimSpace(string(stderr)))
	}
	if got := strings.TrimSpace(string(stdout)); got != want {
		return fmt.Errorf("docker: requested runtime %q but container runs %q — refusing to advertise kernel isolation", want, got)
	}
	return nil
}

// newWithRunner is the test seam: same as New but with an injected runner and a
// pre-resolved id, so unit tests never touch a daemon. The container is assumed
// already "started" by the fake.
func newWithRunner(dir string, opts Options, runner dockerRunner) (*Sandbox, error) {
	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}
	lsb, err := local.New(dir)
	if err != nil {
		return nil, err
	}
	if opts.User == "" {
		opts.User = currentUserSpec()
	}
	// Drive the real `run` path through the fake so tests can assert the argv.
	stdout, _, _, err := runner.Run(context.Background(), runArgs(opts, lsb.RealRoot()), nil)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(stdout))
	return &Sandbox{Sandbox: lsb, runner: runner, opts: opts, id: id}, nil
}

// Capabilities OVERRIDES the embedded local backend's (which reports
// IsolationNone). It reports the real boundary: IsolationKernel under gVisor
// (Runtime=="runsc"), IsolationProcess otherwise, and whether the network is on.
// This is what the MinIsolation policy gate reads to decide if untrusted code may
// run here.
func (s *Sandbox) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Isolation: s.opts.isolation(), Network: s.opts.Network}
}

// Exec runs cmd inside the container via `docker exec`. The contract matches
// local.Exec exactly (Slice 3 conformance): a non-zero exit is a normal *Result,
// not a Go error; err is reserved for the docker machinery failing. cmd.Timeout
// and ctx cancellation both bound the run, whichever fires first, and the kill
// sets Result.TimedOut.
//
// On timeout the in-container `timeout -s KILL <secs>` SIGKILLs the command it
// launched (the common runaway-loop case). It does NOT chase a process that
// deliberately daemonizes itself into a new session — that is what the CONTAINER
// boundary is for: such a process is confined, capped by --pids-limit/--memory,
// and destroyed when Close does `docker rm -f`. The timeout is a liveness lever
// to hand control back to the agent loop; the container is the security boundary
// (cf. the gated policy's "heuristic, not a boundary" note).
func (s *Sandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	if cmd.Path == "" {
		return nil, fmt.Errorf("docker exec: empty command path")
	}
	args, timedOutMarker, err := s.execArgs(cmd)
	if err != nil {
		return nil, err // e.g. cmd.Dir escaped the fence — same as local.Exec returning err.
	}

	runCtx := ctx
	if cmd.Timeout > 0 {
		// A hard ceiling above the in-container `timeout` so a wedged daemon/exec
		// client can't outlive the budget either. The in-container timeout is the
		// primary kill; this ctx is the backstop.
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cmd.Timeout+execTimeoutGrace)
		defer cancel()
	}

	start := time.Now()
	stdout, stderr, exit, err := s.runner.Run(runCtx, args, cmd.Stdin)
	dur := time.Since(start)
	if err != nil {
		// docker exec itself couldn't run. If the ctx expired/was cancelled this is
		// the timeout/cancel path — report it as a timed-out Result (the contract
		// counts ctx kills as TimedOut), not a hard error.
		if runCtx.Err() != nil {
			return &sandbox.Result{ExitCode: -1, Stdout: stdout, Stderr: stderr, Duration: dur, TimedOut: true}, nil
		}
		return &sandbox.Result{ExitCode: -1, Stdout: stdout, Stderr: stderr, Duration: dur}, fmt.Errorf("docker exec: %w", err)
	}

	res := &sandbox.Result{ExitCode: exit, Stdout: stdout, Stderr: stderr, Duration: dur}
	// The budget bit if the ctx expired, or the in-container `timeout` SIGKILLed the
	// command (exit 137 = 128+9). NOTE: an OOM-kill (--memory) ALSO surfaces as 137,
	// so under a timeout budget the two are indistinguishable by exit code alone and
	// an OOM may be reported as TimedOut — an accepted imprecision (the model just
	// sees "the command was killed", which is true either way).
	if runCtx.Err() != nil || (timedOutMarker && exit == timeoutKillExit) {
		res.TimedOut = true
	}
	return res, nil
}

// Close stops and removes the container. It is idempotent (a second call, or a
// call after a failed start, is a no-op) and best-effort — a leaked container is
// worse than a noisy error, so we always attempt the removal. Because the
// container was started with --rm, `docker rm -f` both kills and removes it.
func (s *Sandbox) Close() error {
	if s == nil || s.id == "" {
		return nil
	}
	id := s.id
	s.id = "" // idempotent: a second Close does nothing.
	// Use a fresh, bounded context: Close commonly runs in a defer AFTER the run's
	// ctx was cancelled, and we must still reap the container.
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_, stderr, exit, err := s.runner.Run(ctx, []string{"rm", "-f", id}, nil)
	if err != nil {
		return fmt.Errorf("docker: removing container %s: %w", id, err)
	}
	if exit != 0 {
		return fmt.Errorf("docker: removing container %s: exit %d: %s", id, exit, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// timing/exit constants for Exec and Close.
const (
	// execTimeoutGrace is how much longer the outer ctx waits than the in-container
	// `timeout`, so the inner kill (which reaps the process group) is what fires,
	// not the outer ctx (which would only kill the exec client).
	execTimeoutGrace = 5 * time.Second
	// timeoutKillExit is `timeout`'s exit code when it had to SIGKILL the command
	// (128 + 9). Seeing it means the command was killed for exceeding its budget.
	timeoutKillExit = 137
	// closeTimeout bounds container teardown so a hung daemon can't wedge Close.
	closeTimeout = 30 * time.Second
)

// runArgs builds the `docker run -d` argv that starts the long-lived container.
// Every security flag from §6 is here, each closing a specific escape — keep the
// comments; they are the security rationale, not decoration.
func runArgs(opts Options, hostDir string) []string {
	args := []string{
		"run", "-d", // detached: New returns once the container id is printed.
		"--rm", // auto-remove once the container STOPS. Note: this does not prevent a
		// leak if the agent is SIGKILLed while the container is still running — the
		// container keeps running until reaped. That's what the --label is for:
		// `docker ps -aq --filter label=driver-os-sandbox` finds running leaks.
		"--init", // PID 1 is a real init that REAPS ZOMBIES. Orphaned descendants of a
		// `docker exec` reparent to PID 1; without --init they'd accumulate as zombies
		// against --pids-limit. (--init reaps the dead; it does not kill live
		// background processes — the resource caps + Close do that.)
		"--label", containerLabel + "=1", // tag for finding/reaping leaked containers.
	}
	if opts.Runtime != "" {
		// --runtime=runsc is the gVisor graduation (IsolationKernel) — the one flag
		// (D4). Empty leaves the daemon default (runc, IsolationProcess).
		args = append(args, "--runtime="+opts.Runtime)
	}
	// --network none: no exfiltration / no callbacks. The default; opt-in to enable
	// (e.g. for a trusted dep-fetch phase). Untrusted code MUST stay network-off.
	if opts.Network {
		args = append(args, "--network", "bridge")
	} else {
		args = append(args, "--network", "none")
	}
	args = append(args,
		"--user", opts.User, // non-root in-container AND host-owned bind-mount writes (§6/§13 uid gotcha).
		"--cap-drop", "ALL", // drop every Linux capability.
		"--security-opt", "no-new-privileges", // block setuid escalation.
		"--read-only",     // root filesystem is immutable...
		"--tmpfs", "/tmp", // ...except a scratch /tmp...
		// ...and the workspace, the ONLY rw mount. --mount (not -v) so a host path
		// containing ':' can't misparse into bogus mount options/targets; src/dst are
		// explicit key=value fields.
		"--mount", "type=bind,src="+hostDir+",dst="+opts.WorkdirMount,
		"-w", opts.WorkdirMount,
	)
	// Extra (caller-owned) mounts — e.g. a read-only host GOMODCACHE so an
	// in-container `go build` resolves modules with the network off (§13).
	for _, m := range opts.ExtraMounts {
		args = append(args, "-v", m)
	}
	// Resource caps: contain fork bombs / memory bombs / CPU spin (§6).
	if opts.Memory != "" {
		args = append(args, "--memory", opts.Memory)
	}
	if opts.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(opts.PidsLimit))
	}
	if opts.CPUs != "" {
		args = append(args, "--cpus", opts.CPUs)
	}
	// The image and the keep-alive command. `sleep infinity` holds the container
	// open for its lifetime; commands arrive via `docker exec` (D3).
	args = append(args, opts.Image, "sleep", "infinity")
	return args
}

// execArgs builds the `docker exec` argv for one Command and reports whether the
// command was wrapped with an in-container timeout (so Exec knows to interpret a
// 137 exit as TimedOut). It returns an error if cmd.Dir escapes the fence —
// matching local.Exec, which refuses an out-of-root Dir.
func (s *Sandbox) execArgs(cmd sandbox.Command) (args []string, timed bool, err error) {
	workdir, err := s.containerWorkdir(cmd.Dir)
	if err != nil {
		return nil, false, err
	}

	args = []string{"exec"}
	if len(cmd.Stdin) > 0 {
		args = append(args, "-i") // attach stdin so a piped script reaches the process.
	}
	args = append(args, "-w", workdir)
	// Base env (opts.ExtraEnv) then per-command env on top (Command.Env wins).
	for _, e := range s.opts.ExtraEnv {
		args = append(args, "-e", e)
	}
	for _, e := range cmd.Env {
		args = append(args, "-e", e)
	}
	args = append(args, s.id)

	// Build the program + args, optionally under an in-container `timeout`.
	prog := append([]string{cmd.Path}, cmd.Args...)
	if cmd.Timeout > 0 {
		// Round UP to whole seconds (timeout's granularity) so a 1500ms budget waits
		// 2s, never floors to 1s and kills early; floor of 1s for any sub-second.
		secs := int(math.Ceil(cmd.Timeout.Seconds()))
		if secs < 1 {
			secs = 1
		}
		// -s KILL: hard-kill (untrusted code may ignore SIGTERM).
		args = append(args, "timeout", "-s", "KILL", strconv.Itoa(secs))
		args = append(args, prog...)
		return args, true, nil
	}
	args = append(args, prog...)
	return args, false, nil
}

// containerWorkdir maps a sandbox-relative cmd.Dir to its in-container path,
// FENCED the same way local.Exec fences it: the dir must resolve inside the
// workspace root (lexically and through symlinks), or it's refused. The relative
// path is then joined under the in-container workspace mount. "" means the root.
func (s *Sandbox) containerWorkdir(dir string) (string, error) {
	if dir == "" {
		return s.opts.WorkdirMount, nil
	}
	abs, err := s.RelInRoot(dir) // host-side fence check (lexical + symlink-safe).
	if err != nil {
		return "", err
	}
	if abs == "." {
		return s.opts.WorkdirMount, nil
	}
	return s.opts.WorkdirMount + "/" + abs, nil
}
