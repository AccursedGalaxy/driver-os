# driver-os

[![CI](https://github.com/AccursedGalaxy/driver-os/actions/workflows/ci.yml/badge.svg)](https://github.com/AccursedGalaxy/driver-os/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/AccursedGalaxy/driver-os.svg)](https://pkg.go.dev/github.com/AccursedGalaxy/driver-os)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

driver-os is a headless coding agent in Go: a think→act→observe loop with
typed run outcomes, sandbox isolation tiers, verification gates, per-role cost
accounting, and optionally self-signed proof bundles. It runs any model
behind one provider interface. The design premise is that an agent's "done"
is worthless until an external oracle agrees, so every run ends in a typed
outcome decided by gates, not by the model's own claim.

What this repository releases, in order of stability:

1. **Core library** (`llm/`, `provider/`, `sandbox/`, `memory/`): provider
   abstraction modeled on the `database/sql` driver pattern, the sandbox
   interfaces, and the long-term-memory contract. The most stable surface.
2. **Execution engine** (`agent/`, `runspec/`, `profile/`, `headless/`,
   `cmd/runner`): the agent loop, its gates and detectors, the typed outcome
   contract, evidence bundles, and a small reference runner.
3. **GoBench validator (preview)** (`cmd/gobench-validate`,
   `eval/suite/gobench`): the instance validation pipeline for a Go agent
   benchmark that grades honesty as well as resolve, with five canonical
   demonstration fixtures.

The TUI, long-term memory backend, multi-model council, routing experiments,
issue bot, and the model research that shaped these defaults live outside this
repository; this repo is deliberately narrow. See [DESIGN.md](DESIGN.md) for
the library spec and the reasoning behind each decision.

## Supported providers

| Provider          | Adapter         | Env var              | `-provider` value | Status |
|-------------------|-----------------|----------------------|-------------------|--------|
| OpenAI            | `openaicompat`  | `OPENAI_API_KEY`     | `openai`          | ✅ |
| OpenRouter        | `openaicompat`  | `OPENROUTER_API_KEY` | `openrouter`      | ✅ |
| X.AI (Grok)       | `openaicompat`  | `X_AI_API_KEY`       | `xai`             | ✅ |
| Local (Ollama, …) | `openaicompat`  | (keyless)            | `ollama`          | ✅ |
| Claude            | `anthropic`     | `ANTHROPIC_API_KEY`  | `anthropic`       | ✅ |

The first four speak the OpenAI Chat Completions wire format, so a single
adapter covers them. Claude uses its own native Messages API via the
`anthropic` adapter (DESIGN.md, decision 3): signed-thinking replay, the
effort knob, and prompt caching. The runner selects any of the five with
`-provider`.

## Quick start (library)

```go
reg := llm.NewRegistry()
reg.Add("grok",       openaicompat.XAI("grok-4-fast"))
reg.Add("openrouter", openaicompat.OpenRouter("openai/gpt-4o-mini"))

resp, err := reg.MustGet("grok").Generate(ctx, llm.Request{
    Messages:  []llm.Message{llm.User("Explain goroutines in one sentence.")},
    MaxTokens: 200,
})
fmt.Println(resp.Text(), resp.Usage.TotalTokens)
```

## The reference runner

`cmd/runner` is the headless loop with no optional capabilities armed: no
escalation ladder, no reviewer or planner roles, no pricing table. It exists
to demonstrate the run contract with the smallest possible dependency
surface.

```sh
go run ./cmd/runner -trust trusted-local -task "What module path does this project declare?"
```

### Typed outcomes

A run never ends in prose. It ends in one value of a closed outcome enum,
decided by the harness (verify command, diff requirements, loop detectors,
resource caps), and the exit code carries it:

`0` answered · `2` unverified · `3` resource cap (iterations/wall/context/
budget) · `4` stuck (a loop detector fired) · `5` provider/transport error ·
`6` refused on policy · `7` canceled by caller · `8` scope violation
(`-diff-scope`) · `1` setup error.

`answered` means the harness accepted a final answer. When a verify command
is configured, `answered` additionally requires that command to have run
green immediately before the answer was accepted; without one, `answered`
carries no correctness evidence. Check `guarantees.verification.status` in
the result rather than treating the outcome name as a verification claim. A
run rescued by a closing gate records what it was rescued from
(`rescued_from`). Branch on `$?` to retry, escalate, or give up.

### Driving it from scripts

The runner obeys the Unix contract, so it composes in a pipe, a Makefile, or
CI (full contract in
[docs/specs/CLI-SCRIPTABLE.md](docs/specs/CLI-SCRIPTABLE.md)):

- **`-format text|json|ndjson`**: `text` (default) prints the answer + a
  `SUMMARY` line for humans; `json` emits one result object; `ndjson` streams
  one event per turn ending in a terminal `result` event. **stdout is the
  data channel; the live trace and banners always go to stderr**, so
  `runner -format=json … | jq .answer` just works.
- **`-verify-cmd`** is the single highest-leverage flag: it is the external
  oracle that decides `answered`. `-test-fence` keeps the agent from editing
  the tests that gate it (a canonical Go/Python fence auto-arms when a verify
  command is set).
- **Headless defaults favor unattended runs**: inside a git repo the run
  isolates itself in a throwaway worktree (`-worktree` defaults to `auto`;
  changes come back as a banked `<run-id>.patch`), and reasoning effort
  defaults to `low` (`-effort=default` restores the provider default).
- **`-task -`** reads the task from stdin: `cat issue.md | runner -task -`.
- `-trace=compact` reduces the stderr trace to one line per iteration plus
  gate milestones; `-report out.md` writes a one-read markdown report.

```sh
# machine-readable, fully unattended:
echo "what is the module path?" \
  | runner -task - -format=json -provider=openrouter -model=openai/gpt-4o-mini
# stream progress events, keep only the final result:
runner -format=ndjson -task "run the tests, report failures" | jq -c 'select(.type=="result")'
```

## Proof bundles

Every completed headless run with transcript persistence writes
`<run-id>.bundle/` beside its transcript. The canonical `manifest.json`
separates reproducible artifacts (patch, transcript, and captured verifier
output) from harness attestations and hashes every component. Verification
is offline only: it checks hashes and the signature and never executes
anything recorded in the bundle.

```sh
runner -format=json -task "fix the test" | jq '{bundle_path,bundle_manifest_sha256}'
runner bundle verify ~/.local/share/driver-os/runs/<run-id>.bundle
```

Set `DRIVER_BUNDLE_SIGNING_KEY` to a base64 or hex Ed25519 seed/private key
to sign newly produced bundles. Signatures are currently self-signed: a
valid signature proves the manifest was signed by the key embedded in that
same manifest, not that the signer is anyone in particular — verification
does not yet take a trusted-key or expected-fingerprint input. No
credentials or environment dump are included. Bundle-write failures are
warnings and never change the run outcome. A bundle proves artifact
integrity and records verification evidence; it does **not** prove that the
verifier fully captures task correctness.

## Running untrusted code (sandbox backends)

Every effect the agent causes (running a command, reading or writing a file)
flows through one `sandbox.Sandbox` boundary (see `docs/specs/SANDBOX.md`).
The backend, not the tool, decides how strongly that boundary isolates:

| `-sandbox` / `-runtime` | isolation | use for |
|---|---|---|
| `local` *(default)* | none: host subprocess + path fence | code **we** wrote and trust |
| `docker` / `runc` | process: container, shared host kernel | isolated-but-not-hostile |
| `docker` / `runsc` | kernel: gVisor userspace kernel | **arbitrary, model-authored code** |

```sh
# Locked-down container (network off, root fs read-only, resource caps, non-root):
go run ./cmd/runner -sandbox=docker -task "..."

# Treat the task's code as HOSTILE. The named profile is authoritative and
# normalizes sandbox, runtime, network, secrets, worktree, and instruction policy:
go run ./cmd/runner -trust untrusted -task "..."
```

Build the container image once (it carries `sh`, `rg`, `git`, `go`):

```sh
make sandbox-image        # builds driver-os-sandbox:latest
make sandbox-integration  # runs the docker-backed tests against a real daemon
```

Network is off by default (`--network none`) so untrusted code can't
exfiltrate; pass `-network` to allow egress. The workspace is the only
writable mount, and the path fence rejects planted symlink escapes (the
confused-deputy guard in `sandbox/local`); it is check-then-use, so it does
not close all concurrent symlink races — details and residual risk in
`docs/specs/SANDBOX.md`.

The gVisor tier is supported in code and covered by unit tests, but its
real-host integration gate (`-trust untrusted` yields kernel isolation; a
missing `runsc` refuses rather than silently downgrades) has not yet been
validated on a gVisor host — treat it as integration-validation pending.

### Execution profiles

Orthogonal to trust, `-profile` names the run's behavior defaults (iteration
caps, effort, worktree, verify posture) as one immutable, versioned identity
recorded in every transcript, so "driver-os + model X" names a reproducible
configuration. Explicitly-set flags override the profile (the transcript
records each field's source and whether the run stayed `canonical`), and
trust floors can only tighten a profile, never the reverse. Details:
`docs/specs/PROFILES.md`.

## GoBench validator (preview)

`cmd/gobench-validate` is the instance validation pipeline for a rolling Go
agent benchmark whose scoring axis is honesty as well as resolve: does the
agent's claimed outcome match what an external grader can reproduce? The
pipeline checks schema and toolchain pins, reproduces the red/green oracle,
scrubs problem statements, and screens for solution leakage.

Five canonical demonstration fixtures ship in
`eval/suite/gobench/testdata/instances/` (urfave/cli, OPA, Prometheus,
Lipgloss, Dolt). **These are a preview**: they demonstrate the instance
format and the validation gates, and they are burned for scoring purposes
precisely because they are public. The benchmark's real instance sets are
mined fresh per release and stay private until publication.

## Develop

```sh
go vet ./... && go test -race ./...   # the repo gate (deterministic, no network)
```

## Status & stability

This is a v0.x beta and the API and CLI surface are still moving, so expect
breaking changes between minor versions until v1. The narrow scope is
deliberate: the run contract, the gates, and the bundle format are the parts
built to be depended on. The experimentation that pressure-tests them
happens in companion repositories and lands here only after it survives.

## License

[MIT](LICENSE) © Robin Bohrer
