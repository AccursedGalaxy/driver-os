<div align="center">

# driver-os — verifiable coding agents

### Ship code that passed a check, not code that merely says it did.

[![CI](https://github.com/AccursedGalaxy/driver-os/actions/workflows/ci.yml/badge.svg)](https://github.com/AccursedGalaxy/driver-os/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/AccursedGalaxy/driver-os.svg)](https://pkg.go.dev/github.com/AccursedGalaxy/driver-os)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**External verification** · **Sandboxed execution** · **Typed outcomes** · **Signed proof bundles** · **Any model**

An open-source, headless coding-agent harness in Go that refuses to take the
model's word for "done." driver-os runs OpenAI, Anthropic Claude, OpenRouter,
X.AI, and local Ollama models behind one provider interface, then grades their
work with external test gates. Every run produces a scriptable outcome, tracks
cost by role, and can emit a signed proof bundle for offline verification.

[Run the false-green demo](#see-it-catch-a-false-green) · [Quick start](#04--quick-start) · [Use it from scripts](#06--driving-it-from-scripts) · [Read the design](DESIGN.md)

</div>

---

## 01 · The thesis

Coding agents are graded on their own word: the model declares a task
green, the harness believes it, and the number goes up. Some of those greens
are false: the code doesn't compile, the test never ran, the fix quietly
regressed something else. driver-os is built on the opposite premise: **an
agent's "done" is worthless until an external oracle agrees.** Every run
ends in a typed outcome decided by gates (a verify command, diff
requirements, loop detectors, resource caps), never by the model's own
claim.

| verdict | meaning |
|---|---|
| **true green** | An external check re-ran the work and it holds. The only green that should count. |
| **false green** | The agent reported success; independent verification disagrees. This is the failure mode the whole harness is built to expose. |

## See it catch a false green

Run the same deterministic agent twice. It writes the wrong answer and says
all checks pass both times. The first run trusts that claim. The second adds
one external gate and exits `2: unverified`, then verifies both proof bundles
offline.

```sh
make demo-false-green
```

The demo needs Go and Git, but no API key, network access, Docker, or `jq`. It
runs in a temporary directory and removes its files when finished.

## 02 · The system, in five pieces

1. **One contract.** A run never ends in prose. It ends in one value of a
   closed outcome enum, carried in the exit code, so scripts can branch on
   it.
2. **One oracle seam.** `-verify-cmd` is the external command that decides
   `answered`; `-test-fence` keeps the agent from editing the tests that
   gate it.
3. **One auditable artifact.** Every completed run writes a proof bundle:
   hashed, optionally signed, verifiable offline.
4. **One effect boundary.** Every command and file access flows through a
   single `sandbox.Sandbox` interface, with backends ranging from a bare
   subprocess to a gVisor userspace kernel.
5. **One benchmark seed (preview).** The validation pipeline for a Go
   benchmark that grades honesty as well as resolve.

What this repository releases, in order of stability:

| layer | packages | stability |
|---|---|---|
| Core library | `llm/`, `provider/`, `sandbox/`, `memory/` | most stable: provider abstraction on the `database/sql` driver pattern, sandbox interfaces, memory contract |
| Execution engine | `agent/`, `runspec/`, `profile/`, `headless/`, `cmd/runner` | the loop, gates, detectors, outcome contract, bundles, reference runner |
| GoBench validator | `cmd/gobench-validate`, `eval/suite/gobench` | preview |

The TUI, long-term memory backend, multi-model council, routing experiments,
issue bot, and the model research that shaped these defaults live outside
this repository; this repo is deliberately narrow. [DESIGN.md](DESIGN.md)
has the library spec and the reasoning behind each decision.

## 03 · Run any model

| Provider          | Adapter         | Env var              | `-provider` value | Status |
|-------------------|-----------------|----------------------|-------------------|--------|
| OpenAI            | `openaicompat`  | `OPENAI_API_KEY`     | `openai`          | ✅ |
| OpenRouter        | `openaicompat`  | `OPENROUTER_API_KEY` | `openrouter`      | ✅ |
| X.AI (Grok)       | `openaicompat`  | `X_AI_API_KEY`       | `xai`             | ✅ |
| Local (Ollama, …) | `openaicompat`  | (keyless)            | `ollama`          | ✅ |
| Claude            | `anthropic`     | `ANTHROPIC_API_KEY`  | `anthropic`       | ✅ |

The first four speak the OpenAI Chat Completions wire format, so one adapter
covers them. Claude gets its own native Messages adapter (DESIGN.md,
decision 3): signed-thinking replay, the effort knob, and prompt caching.
The runner selects any of the five with `-provider`.

## 04 · Quick start

As a library:

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

As a harness: `cmd/runner` is the headless loop with no optional
capabilities armed: no escalation ladder, no reviewer or planner roles, no
pricing table. It exists to demonstrate the run contract with the smallest
possible dependency surface.

```sh
go run ./cmd/runner -trust trusted-local -task "What module path does this project declare?"
```

## 05 · Typed outcomes

The exit code carries the outcome, so a pipe, Makefile, or CI step can
branch on `$?` to retry, escalate, or give up:

| exit | outcome | decided by |
|---|---|---|
| `0` | answered | harness accepted a final answer |
| `2` | unverified | answer given, no green verify evidence |
| `3` | resource cap | iterations / wall clock / context / budget |
| `4` | stuck | a loop detector fired |
| `5` | provider error | transport or upstream failure |
| `6` | refused | model declined on policy |
| `7` | canceled | caller canceled the run |
| `8` | scope violation | diff left `-diff-scope` |
| `1` | setup error | run never started |

`answered` means the harness accepted a final answer. When a verify command
is configured, `answered` additionally requires that command to have run
green immediately before the answer was accepted; without one, `answered`
carries no correctness evidence. Check `guarantees.verification.status` in
the result rather than treating the outcome name as a verification claim. A
run rescued by a closing gate records what it was rescued from
(`rescued_from`).

## 06 · Driving it from scripts

The runner obeys the Unix contract (full contract in
[docs/specs/CLI-SCRIPTABLE.md](docs/specs/CLI-SCRIPTABLE.md)):

- **`-format text|json|ndjson`**: `text` (default) prints the answer + a
  `SUMMARY` line for humans; `json` emits one result object; `ndjson`
  streams one event per turn ending in a terminal `result` event. **stdout
  is the data channel; the live trace and banners always go to stderr**, so
  `runner -format=json … | jq .answer` just works.
- **`-verify-cmd`** is the single highest-leverage flag: it is the external
  oracle that decides `answered`. `-test-fence` keeps the agent from editing
  the tests that gate it (a canonical Go/Python fence auto-arms when a
  verify command is set).
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

## 07 · Proof bundles

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
same manifest, not that the signer is anyone in particular; verification
does not yet take a trusted-key or expected-fingerprint input. No
credentials or environment dump are included. Bundle-write failures are
warnings and never change the run outcome. A bundle proves artifact
integrity and records verification evidence; it does **not** prove that the
verifier fully captures task correctness.

## 08 · Running untrusted code

Every effect the agent causes (running a command, reading or writing a
file) flows through one `sandbox.Sandbox` boundary (see
[docs/specs/SANDBOX.md](docs/specs/SANDBOX.md)). The backend, not the tool,
decides how strongly that boundary isolates:

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
not close all concurrent symlink races. Details and residual risk are in
`docs/specs/SANDBOX.md`.

The gVisor tier is supported in code and covered by unit tests, but its
real-host integration gate (`-trust untrusted` yields kernel isolation; a
missing `runsc` refuses rather than silently downgrades) has not yet been
validated on a gVisor host. Treat it as integration-validation pending.

### Execution profiles

Orthogonal to trust, `-profile` names the run's behavior defaults (iteration
caps, effort, worktree, verify posture) as one immutable, versioned identity
recorded in every transcript, so "driver-os + model X" names a reproducible
configuration. Explicitly-set flags override the profile (the transcript
records each field's source and whether the run stayed `canonical`), and
trust floors can only tighten a profile, never the reverse. Details:
[docs/specs/PROFILES.md](docs/specs/PROFILES.md).

## 09 · GoBench validator (preview)

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
