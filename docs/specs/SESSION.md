# Session container — direction note

Status: **Slice 1 (stateful `Session.Exec`) SHIPPED. Slice 2 (`ProcessHost`)
designed — spec below.** This note records the decided design so we don't
relitigate it. It is the foundation slice of the session-container build order
(see `docs/specs/CODE-INTELLIGENCE.md` for the whole arc):

1. **`Session` — stateful Exec** *(shipped)* — successive `Exec` calls share
   cwd + environment.
2. **`ProcessHost`** *(designed — see "Slice 2" below)* — an optional capability
   for long-lived processes (`StartProcess` → held-open stdin/stdout + bounded
   stderr + `Wait`/`Kill`).
3. **gopls driver + symbol tools** *(first tenant)* — JSON-RPC over the process
   host. Not built yet.

`Sessioner`/`Session` are already stubbed-as-planned in `sandbox/sandbox.go`
(optional capability, discovered by type-assertion like `LimitedReader`).

## Honest framing — what a Session actually buys (and what it doesn't)

The earlier "warm `GOCACHE` across turns" justification **does not hold for the
session specifically**: the docker backend is *already* one long-lived container
per run (`sleep infinity` + `docker exec` per `Exec`), and `GOCACHE`/`GOMODCACHE`
already point at the per-container `/tmp` tmpfs (see the Dockerfile). So every
`go build`/`go test` in a run is *already warm* — at the **container** level, with
no session involved. A `Session` adds nothing there.

What a `Session` genuinely adds in slice 1:

- **Shell-state persistence** — `cd` and `export` carry across `Exec` calls
  (modest direct value; a model that `cd`s once stays there).
- **The architectural seam** — `Sessioner` → `ProcessHost` → gopls. This is the
  *real* reason to build it now: gopls (slice 3) needs a long-lived host, and the
  `Session` is where that host will live. Slice 1 establishes the seam so slices 2
  and 3 attach with no reshape.

We write this down so the "foundation = warm cache" narrative (already retracted
in `docs/specs/CODE-INTELLIGENCE.md`) doesn't creep back.

## The mechanism — shell-wrapper state file (decided)

`docker exec` is **stateless**: each exec is a fresh process at the container's
default cwd with the base env. To make `Session.Exec` stateful without a
long-lived shell process (deferred to slice 2), each shell command is wrapped:

```
restore  ─ source a per-session env file; cd to the saved cwd
run      ─ eval the model's command IN this shell (so its cd/export take effect)
capture  ─ write the new cwd and `export -p` back to the state file
```

The state file lives in the container's `/tmp` tmpfs (persists for the container's
lifetime = the run). Key points, each a deliberate decision:

- **`eval` the command in-process, not `"$@"` as a child.** The run tool already
  builds `sh -c <string>`; the session runs that `<string>` via `eval` *in the
  wrapper shell*, so an `export`/`cd` inside it mutates the wrapper shell and is
  captured. Running it as a child process would lose the state (env changes don't
  propagate to the parent). Semantically identical to `sh -c <string>` otherwise.
- **Command body + state-dir passed via env (`__SESSION_BODY`, `__SESSION_ST`,
  `__SESSION_RESTORE`), never interpolated into the script.** The wrapper script is
  a static constant → no shell-injection surface from the model's command. The
  internals are `unset` before the `export -p` dump so they don't pollute session
  state.
- **Only shell `-c` commands are wrapped.** A non-shell `Exec` (`rg`, `git` run
  directly) is a single process that can't carry session state, so it passes
  straight through to the base `Exec`. Session state is a property of the *run
  tool* surface.
- **Explicit `cmd.Dir` resets the session cwd.** With `cmd.Dir` set, the wrapper
  skips the saved-cwd restore (the backend's `-w` puts it there); the post-command
  `pwd` capture then makes that the new session cwd. With `cmd.Dir` empty, the
  saved cwd is restored. Documented, predictable.
- **Restore is best-effort (`2>/dev/null`, no `set -e`).** A missing state file, a
  vanished saved cwd, or a re-`export` of a read-only var must not abort the
  command. Container shell is busybox `ash` (alpine image); host shell for the
  local backend is whatever `/bin/sh` is — the script stays POSIX.

The same `sandbox/session.New(base, stateDir)` serves **both** backends; only the
state-dir path differs (docker: `/tmp/...`; local: `os.TempDir()/...`).

## Verify/diagnose run on the BASE sandbox, NOT the session (decided)

The closing verification gate (`VerifyCmd`), the upgrade check, and the
stuck-diagnostics feed (`DiagnoseCmd`) **must** run in a clean context:

- A model that `cd`s into a subdir would otherwise make a session-routed
  `go build ./...` run from the wrong place.
- The session's accumulated env (arbitrary model `export`s) could change what the
  verification command does — and the gate must be trustworthy.

Both run on the same warm container regardless (warmth is container-level), so
there is no cache cost. Implemented as `Config.VerifySandbox` (nil ⇒ `Sandbox`,
preserving today's behavior for every session-off caller). This **intentionally
contradicts** the older `session-container-slice` memory note that said "point the
diagnostics feed at the session" — that instruction predated the warm-cache
correction above.

## Wiring — opt-in, in `cmd/agent/main.go`

`-session` (off by default). Session-on builds two chains over the one base
sandbox:

- **tool sandbox** = `gated(sessioned(base))` — the model's `run`/file/search tools.
- **verify sandbox** = `gated(base)` — `VerifyCmd`/`DiagnoseCmd`.

The session sits **below** the gate so the gate's policy/allowlist and the human
prompt see the model's *real* command, not the wrapper script. The session is
`defer`-Closed before the base container is torn down (LIFO).

Wiring lives in `main` (not `Run`/`RunNative`) because the gate-ordering above
needs the session inserted beneath an already-applied gate, and because **eval
sweeps stay session-off** by construction (they build `Config` directly and never
opt in) — exactly what the slice memo wanted.

## Test plan

`sandboxtest.RunSessionConformance(t, factory)` — one suite, both backends (local
always; docker behind `docker_integration`):

- `cd` persists across `Exec`,
- `export` persists across `Exec`,
- two sessions are isolated from each other,
- a non-shell command passes through,
- an explicit `cmd.Dir` overrides the session cwd.

---

# Slice 2 — `ProcessHost` (designed, not yet built)

Designed backward from its **only** near-term tenant: a persistent **gopls**
spoken to as **JSON-RPC over stdio**. That, and nothing more, sets the
requirements: a continuously writable stdin, a continuously readable stdout (the
client parses Content-Length-framed messages), stderr that can't deadlock the
server, a reliable hard-kill of the *in-container* process, and a way to learn it
**died on its own**. **LSP framing lives in the slice-3 driver, not here** —
`ProcessHost` is a protocol-agnostic transport primitive ("start a long-lived
process, hold its streams, kill it"). That is the seam that keeps the driver
backend-agnostic.

## The interface (decided)

Optional capabilities on `sandbox.Sandbox`, discovered by type-assertion exactly
like `Sessioner`/`LimitedReader`:

```go
// ProcessHost: start a long-lived process and hold its streams open, beyond Exec's
// run-to-completion. Optional; type-assert to discover it.
type ProcessHost interface {
    StartProcess(ctx context.Context, cmd Command) (Process, error)
}

// Process is a running long-lived process. stdin/stdout are raw streams the caller
// drives; stderr is drained internally so the process can never stall on a full
// pipe. Always Kill it (defer).
type Process interface {
    Stdin() io.Writer        // client → process; closing it (via Kill) gives the process EOF.
    Stdout() io.Reader       // process → client; the caller reads this continuously.
    StderrSnapshot() []byte  // capped TAIL of stderr, drained internally — for crash diagnostics.
    Wait() error             // blocks until exit; MEMOIZED — safe to call repeatedly/concurrently.
    Kill() error             // idempotent terminate.
}
```

Decided forks (2026-06-03):

- **Input = reuse `sandbox.Command`** (not a new `ProcessSpec`). `StartProcess`
  **ignores `Stdin` and `Timeout`** (live stdin replaces the byte slice; lifetime is
  caller-managed). Rationale: reuses the tested Dir-fence (`containerWorkdir`) +
  env-threading verbatim; precedent is `os/exec` reusing one `Cmd` for `Run`/`Start`.
- **stderr drained internally** to a bounded ring (last ~N KB), exposed as
  `StderrSnapshot()`. The server can never deadlock on a full stderr pipe — aligns
  with P4 (bound everything; a tool that explodes freezes the loop) and mirrors the
  Exec `cappedBuffer`. stdin/stdout stay raw because the driver actively drives both.
- **Crash signal = memoized `Wait()` + idempotent `Kill()`.** The driver watches for
  spontaneous death with `go func(){ markDead(p.Wait()) }()`. Smallest surface that
  serves both teardown and crash-watch.

## Capability placement + gate (decided)

- **On `sandbox.Sandbox`, NOT `Session`.** gopls launches once with a fixed root and
  a *clean* env — it doesn't want the session's accumulated cwd/`export`s. The
  container *is* the host; `Session` (shell-state) and `ProcessHost` (long-lived
  process) are sibling capabilities on the Sandbox.
- **Bypasses the review gate; the driver gets the BASE sandbox** (same precedent as
  slice 1's verify-on-base). gopls is trusted infra the harness starts, not a model
  action. The `gated`/`sessionSandbox` wrappers don't forward `ProcessHost` (just as
  they drop `LimitedReader`), so the type-assertion only succeeds on base. Slice-3
  wiring passes base, like `VerifySandbox`.

## Implementation notes (the real risks)

- **Robust stream plumbing via explicit `os.Pipe()`, not `StdoutPipe()`/`Wait`.**
  Assign the child ends to `c.Stdin/Stdout/Stderr`, keep our ends, close the child
  ends in the parent after `Start`. This sidesteps the `os/exec` rule that `Wait`
  closes `*Pipe` fds and races an in-flight reader — `Wait` does **not** close
  caller-provided files. Closing our stdin end propagates EOF to the process.
- **docker hard-kill (the gotcha):** killing the local `docker exec` client does
  **not** kill the in-container process — it orphans it. So launch via
  `sh -c 'echo $$ >"$PIDFILE"; exec "$@"'` (the `exec` makes `$$` the *target's*
  stable pid), and `Kill()` = `docker exec <id> kill -KILL <pid>` (read the pidfile
  with `docker exec cat`), then close stdin, then kill the local CLI as
  belt-and-suspenders. The container's `rm -f` at teardown is the ultimate backstop.
  Stream with `docker exec -i` (NOT `-t` — a TTY cooks/merges the streams and
  corrupts framed LSP data). `Wait()` = the CLI's exit, which tracks the container
  process's exit (so a gopls crash surfaces as `Wait` returning).
- **local leak-safety:** local has no `rm -f` backstop, so a `Process` the driver
  forgets to `Kill()` would leak a host process. The local backend starts each
  process in its own group (`Setpgid`) and `Sandbox.Close()` kills tracked live
  children (group-kill). Docker gets this free from container teardown. A
  `ctx`-cancel watcher also calls `Kill` so a cancelled run reaps the process.
- **Shared impl in `sandbox/proc`** (like `sandbox/session`): the stream-holding +
  bounded-stderr-drain + memoized-`Wait` + idempotent-`Kill` logic lives once,
  parameterized by the backend's `wait`/`kill` closures and the three stream ends.

## Scope + test plan

Slice 2 is the **transport primitive only** — NO gopls, NO LSP readiness handshake,
NO Dockerfile gopls / `XDG_CACHE_HOME` bump (all slice 3). Shippable on its own,
exactly as slice 1 shipped the `Session` capability without its tenant.

`sandboxtest.RunProcessConformance(t, factory)` — one suite, both backends (local
always; docker behind `docker_integration`):

- start `cat`, write a line to stdin, read the same line back from stdout;
- `Kill` terminates it and `Wait` then returns (and the host/in-container process is
  actually gone);
- a process that exits on its own (`sh -c 'exit 3'`) makes `Wait` return promptly;
- stderr is captured into `StderrSnapshot()` without deadlocking the process
  (`sh -c 'echo oops >&2; cat'`).
