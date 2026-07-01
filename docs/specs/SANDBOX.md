# Sandbox / code-execution isolation — direction note

Status: **Docker/gVisor backend SHIPPED; Firecracker still someday.** The
`Sandbox` interface, the `local` (IsolationNone) and `docker` (IsolationProcess,
or IsolationKernel via `--runtime=runsc`) backends, the conformance suite, and the
`MinIsolation` policy gate all exist. This note records the decided *direction* so
we don't re-research it. Sandboxing is filed under engineering (see
`HARD-PROBLEMS.md` appendix: path sandbox), not the research backlog.

## Where we are today

The agent runs **our own, trusted toolset** (`list_dir`, file ops, shell) behind
a **path fence** (`sandbox/local`'s `resolve` — abs+clean+prefix-check, plus a
symlink-safe escape check so a planted symlink can't redirect a host-side file op
off-root). That `local` backend is `IsolationNone` and is for **trusted code
only**.

For **arbitrary, model-authored code** (treat it as hostile), the `docker` backend
runs `Exec` inside a locked-down container (network off, read-only root fs,
CPU/memory/pids caps, non-root, all caps dropped), reachable at kernel-strength
isolation with `--runtime=runsc` (gVisor). The `MinIsolation` gate
(`agent.Config`) refuses to start a run on a sandbox weaker than the task
requires — so `-untrusted` never executes code on the host or a shared-kernel
container. See `README.md` ("Running untrusted code") for usage.

## The plan: one interface, swappable backends

Same pattern as the `Provider` interface (driver-os) and the `Store` interface
(mneme): a thin `Sandbox`/`Executor` interface in driver-os, with isolation as a
pluggable backend. **We do not adopt E2B (or any vendor) as a dependency** — we
own the stack; vendors are reference architectures to read, not deps.

Graduation path (weak → strong), all reachable from Go:

| Backend | Isolation | When | Status |
|---|---|---|---|
| subprocess + path fence | none — trusted only | running our own tools | **shipped** (`sandbox/local`) |
| Docker / runc | process, **shared kernel** | first arbitrary-exec tool; we own the image | **shipped** (`sandbox/docker`) |
| **gVisor (`runsc`)** | userspace app-kernel, strong | model runs untrusted code, CPU-only | **shipped** — `-runtime=runsc` (one flag) |
| Firecracker | hardware microVM | multi-tenant hostile at scale; needs KVM/Linux | someday (`_deps/firecracker-go-sdk`) |

The Docker backend drives the `docker` CLI via `os/exec` through an injectable
runner (so unit tests assert the exact `docker run`/`docker exec` argv with **no
daemon**), composes `sandbox/local` for the symlink-safe file fence, and runs one
long-lived container per Sandbox (`docker run -d … sleep infinity` + `docker exec`
per call). Podman works by setting the runner binary to `podman` (CLI-compatible).

## The key insight to not lose: gVisor is a runtime *flag*

Graduating the Docker backend to gVisor is **not a rewrite** — gVisor's `runsc`
is an OCI-compatible runtime. You keep the same Docker backend and run the
container with `--runtime=runsc` (or set it as the daemon's default runtime).
That swaps shared-kernel isolation for userspace-app-kernel isolation by changing
one flag. So the Docker → gVisor step is nearly free; design the Docker backend
so the runtime is configurable from day one.

Trade-off to remember: gVisor (and Daytona, which uses gVisor) **blocks GPU
passthrough**. Fine for CPU code-exec; a wall if we ever need ML/GPU sandboxes —
that's the case for going all the way to Firecracker.

## Reference architectures (cloned, gitignored — see `_deps/`)

- **`_deps/firecracker-go-sdk/`** — official Go API for Firecracker microVMs.
  Start: `machine.go`, `firecracker.go`, `jailer.go` (the security wrapper),
  `example_test.go` (working usage). The strongest-isolation backend, in Go.
- **`_deps/daytona/`** — Go, self-hostable sandbox platform whose isolation layer
  *is* gVisor. The reference for "Go + gVisor + sandbox lifecycle" done well.
  Read `apps/runner/`, `apps/daemon/`, `apps/snapshot-manager/`. Note: gVisor is
  wired at the **runner/runtime layer**, not in app logic — confirming isolation
  is an infra choice, not code.

## Recommendation (current)

**Done.** The `Sandbox` interface, the `local` and `docker` backends (OCI runtime
configurable, so **gVisor** is the one-flag `-runtime=runsc` graduation), the
behavioral conformance suite (`sandbox/sandboxtest`, run against both backends),
and the `MinIsolation` policy gate are all shipped. **Firecracker**
(`firecracker-go-sdk`) remains a someday-adapter for genuinely hostile
multi-tenant code — do **not** build microVM infra until that need is real.

Known residual: the symlink fence is check-then-use, so a symlink swapped in
between resolve and the os op is a TOCTOU window; fully closing it needs `openat2`
with `RESOLVE_BENEATH` (Linux-specific). The current check closes the practical
planted-symlink attack.
