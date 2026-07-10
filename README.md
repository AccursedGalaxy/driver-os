# driver-os

[![CI](https://github.com/AccursedGalaxy/driver-os/actions/workflows/ci.yml/badge.svg)](https://github.com/AccursedGalaxy/driver-os/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/AccursedGalaxy/driver-os.svg)](https://pkg.go.dev/github.com/AccursedGalaxy/driver-os)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

driver-os is an experimental agent operating system in Go: a think→act→observe
coding agent with typed run outcomes, sandbox isolation tiers, verification and
review gates, per-role cost accounting, and an eval harness that grades runs
against external oracles rather than trusting the model's own "done". It runs
any model behind one provider interface, so you can swap providers or race
several at once.

The repository is three layers with different stability promises. Experimental
code depends on the engine, never the reverse:

1. **Core library** (`llm/`, `provider/`, `sandbox/`): provider abstraction
   modeled on the `database/sql` driver pattern, plus the sandbox interfaces.
   The most stable surface.
2. **Execution engine** (`agent/`, `cmd/agent`, `cmd/driver`): the agent loop,
   gates, detectors, transcripts, and the headless + TUI binaries.
3. **Research lab** (`eval/`, `council/`, benchmark tooling, `docs/findings/`):
   the measurement machinery and its results. Changes fast, breaks freely.

See [DESIGN.md](DESIGN.md) for the library spec and the reasoning behind each
decision.

## Supported providers

| Provider          | Adapter         | Env var              | `-provider` value | Status |
|-------------------|-----------------|----------------------|-------------------|--------|
| OpenAI            | `openaicompat`  | `OPENAI_API_KEY`     | `openai`          | ✅ |
| OpenRouter        | `openaicompat`  | `OPENROUTER_API_KEY` | `openrouter`      | ✅ |
| X.AI (Grok)       | `openaicompat`  | `X_AI_API_KEY`       | `xai`             | ✅ |
| Local (Ollama, …) | `openaicompat`  | (keyless)            | `ollama`          | ✅ |
| Claude            | `anthropic`     | `ANTHROPIC_API_KEY`  | `anthropic`       | ✅ |

The first four speak the OpenAI Chat Completions wire format, so a single adapter
covers them. Claude uses its own **native** Messages API via the `anthropic`
adapter (DESIGN.md, decision 3): signed-thinking replay, the 5-family effort
knob, and prompt caching. `cmd/agent` can select any of the five with `-provider`.

## Quick start

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

Run the demo (sends one prompt to every provider whose key is in `.env`):

```sh
go run ./experiments/cmd/playground
go run ./experiments/cmd/playground -prompt "your prompt here"
```

Override model ids without recompiling: `OPENROUTER_MODEL`, `XAI_MODEL`.

## Agent loop (with long-term memory)

`cmd/agent` is a minimal think→act→observe agent over the sandbox tools. It has
**persistent memory across runs** via [mneme](https://github.com/AccursedGalaxy/mneme):
before thinking it recalls facts relevant to the task, and after answering it
stores what it learned. So a second, related run starts already knowing the
answer instead of re-exploring.

```sh
go run ./cmd/agent -task "What module path does this project declare?"
go run ./cmd/agent -task "What is this project's module path?"  # recalls from run 1
go run ./cmd/agent -memory=false ...                            # disable memory
```

### Driving it from scripts

`cmd/agent` obeys the Unix contract, so it composes in a pipe, a Makefile, or CI
(full contract in [docs/specs/CLI-SCRIPTABLE.md](docs/specs/CLI-SCRIPTABLE.md)):

- **`-format text|json|ndjson`**: `text` (default) prints the answer + a `SUMMARY`
  line for humans; `json` emits one result object; `ndjson` streams one event per
  turn ending in a terminal `result` event. **stdout is the data channel; the live
  trace and banners always go to stderr**, so `agent -format=json … | jq .answer`
  just works.
- **Exit codes carry the outcome**: `0` answered · `2` unverified · `3` resource
  cap (iterations/wall/context/budget) · `4` stuck (a loop detector fired) · `5`
  provider/transport error · `6` refused on policy · `7` canceled by caller
  (SIGINT / ctx cancel) · `8` scope violation (`-diff-scope`) · `1` setup error.
  Branch on `$?` to retry, escalate, or give up.
- **Orchestrator conveniences, all native** (no wrapper script needed):
  `-trace=compact` reduces the stderr trace to a one-line-per-iteration
  heartbeat plus gate milestones; `-trace-file` banks the full trace;
  `-report out.md` writes a one-read markdown report (result, answer, diff,
  next steps); every run appends a JSONL record to the delegation ledger
  (`-ledger=false` opts out).
- **Headless defaults favor unattended runs**: inside a git repo the run
  isolates itself in a throwaway worktree (`-worktree` defaults to `auto`;
  changes come back as a banked `<run-id>.patch` — pass `-worktree=false` to
  edit the working tree directly), and the solver's reasoning effort defaults
  to `low` (`-effort=default` restores the provider default).
- **`-provider` / `-model`**: pick the backend and model on the command line
  instead of via env (the `*_MODEL` vars still work as defaults; the flag wins).
- **`-task -`** reads the task from stdin: `cat issue.md | agent -task -`.
- **`-review` non-interactively**: `-approve interactive|policy|never` decides a
  gated `run` (policy = auto-allow the safe allowlist, block the rest), and
  `-review-action prompt|commit|discard|keep` decides the diff at the end.
  Interactive prompts use `/dev/tty`, so they coexist with `-task -` and never
  pollute stdout; with no terminal an interactive policy fails fast instead of
  hanging.

```sh
# machine-readable, fully unattended:
echo "what is the module path?" \
  | agent -task - -format=json -provider=openrouter -model=openai/gpt-4o-mini
# stream progress events, keep only the final result:
agent -format=ndjson -task "run the tests, report failures" | jq -c 'select(.type=="result")'
# scripted edit: auto-allow safe commands, leave the diff unstaged for inspection:
agent -review -approve=policy -review-action=keep -task "fix the failing test"
```

Memory lives in `.agent-memory.db` (pure-Go SQLite, gitignored; delete to
reset) and reuses `OPENROUTER_API_KEY`. It is best-effort: with no key the agent
runs statelessly. Each mneme call is bounded by a 30s timeout.

**Caveats (read before trusting recall):**

- **Self-correcting, but only where re-observed.** The agent runs mneme in its
  `Consolidate` strategy: each store reconciles the new facts against existing
  ones (ADD / UPDATE / DELETE), so a changed fact about a mutable repo *replaces*
  the stale one instead of piling up beside it. Bump the Go version and a later
  grounded run overwrites the old value. The catch: reconciliation only touches
  facts a run actually re-observes, so a fact no run has revisited can still be
  stale. Recalled facts stay labelled possibly-stale in the prompt so the model
  verifies with tools (Principle 4). Treat memory as a strong hint, not gospel.
  (Consolidation costs one extra LLM call per store, only when the scope already
  holds facts to reconcile against.)
- **Only tool-verified answers are stored.** To avoid amplifying a guess or a
  stale recalled fact into a permanent one, the agent stores an answer *only* if
  it observed real state via a tool that run. An answer given purely from recall
  is not written back.
- **The embedding model is pinned to the store.** Stored vectors and query
  vectors must come from the same model. Changing `MNEME_EMBED_MODEL` after facts
  exist silently degrades search (no error), so delete `.agent-memory.db` first.
- **Storing is on the happy path.** After the answer prints, the store does a
  synchronous extraction + embedding call before the run returns, so you see the
  answer and then wait briefly.

## Running untrusted code (sandbox backends)

Every effect the agent causes (running a command, reading or writing a file)
flows through one `sandbox.Sandbox` boundary (see `docs/specs/SANDBOX.md`). The
backend, not the tool, decides how strongly that boundary isolates:

| `-sandbox` / `-runtime` | isolation | use for |
|---|---|---|
| `local` *(default)* | none: host subprocess + path fence | code **we** wrote and trust |
| `docker` / `runc` | process: container, shared host kernel | isolated-but-not-hostile |
| `docker` / `runsc` | kernel: gVisor userspace kernel | **arbitrary, model-authored code** |

```sh
# Run the agent's `run`/`search` commands inside a locked-down container
# (network off, root fs read-only, CPU/memory/pids capped, non-root user):
go run ./cmd/agent -sandbox=docker -task "..."

# Treat the task's code as HOSTILE: require gVisor and refuse to start on
# anything weaker. Fails closed: `-untrusted` without `-runtime=runsc` will not
# run a single command:
go run ./cmd/agent -sandbox=docker -runtime=runsc -untrusted -task "..."
```

Build the container image once (it carries `sh`, `rg`, `git`, `go`):

```sh
make sandbox-image        # builds driver-os-sandbox:latest
make sandbox-integration  # runs the docker-backed tests against a real daemon
```

Notes:

- **Network is off by default** (`--network none`) so untrusted code can't
  exfiltrate. Pass `-network` to allow egress (e.g. a trusted dep-fetch). With the
  network off, in-container `go build`/`go test` resolve modules only from a
  read-only host `GOMODCACHE` mount, exposed via the library `docker.Options`
  (`ExtraMounts`), which the integration tests exercise.
- **The workspace is the only writable mount.** For a genuinely untrusted task,
  point the sandbox at a *throwaway copy*, not your live checkout. The backend
  takes a `dir`; the trust decision is the caller's.
- **The fence is symlink-safe.** A symlink planted inside the workspace by
  in-container code can't redirect a host-side read/write off-root (the
  confused-deputy guard in `sandbox/local`).
- `issue-bot` and the `eval` harness stay on `local` (trusted fixtures); they
  don't pay container startup cost.

## Status

The original library build order (DESIGN.md, decision 11) is **complete**: core
types + registry, the `openaicompat` adapter (chat + `iter.Seq2` streaming), the
native `anthropic` adapter, self-contained dual-schema tools, the `Runner`
tool-exec loop, and the comparison/fan-in harness. All are end-to-end tested.

On top of that foundation the project has grown into an agent-harness research
platform:

- **Agent loop** (`cmd/agent`): think→act→observe over the sandbox tools, with
  cross-run memory ([mneme](https://github.com/AccursedGalaxy/mneme)),
  reasoning-trace round-tripping, per-turn timing, and stuck-detection backed by
  a build-diagnostics feed (see `docs/specs/CODE-INTELLIGENCE.md`).
- **Sandbox** (`docs/specs/SANDBOX.md`, `docs/specs/SESSION.md`): one effect
  boundary, three isolation tiers (local / docker-runc / docker-gVisor), plus a
  long-lived process host and stateful shell sessions.
- **Eval harness** (`eval/`): multiple suites including a dogfood corpus
  regression scored against real human verdicts (see `docs/findings/DOGFOOD.md`).
- **Council** (`docs/specs/COUNCIL.md`): adversarial multi-model review (author ↔
  critic ↔ referee) plus a structured consult/Q&A mode; every run recorded as
  dogfood corpus.
- **Dogfood integrations**: `cmd/commit-msg` (commit-message generator) and
  `cmd/issue-bot`.

The open research backlog lives in `HARD-PROBLEMS.md`.

## Develop

```sh
go test ./...        # deterministic unit tests (no network)
go run ./experiments/cmd/playground   # live round-trip (needs keys in .env)
```

## Status & stability

This is a **v0.1.0 beta** and a personal research platform. The `v0.x` version
line means the API and CLI surface are still moving, so expect breaking changes
between minor versions until v1. It's shared because the pieces are genuinely
useful and the research process is out in the open; it is not (yet) a hardened
product with compatibility guarantees.

## License

[MIT](LICENSE) © Robin Bohrer
