# driver-os

A small Go library for experimenting with LLM **model behavior** across
providers behind one uniform interface — swap providers freely, or run several
at once to compare/race them. Modeled on the `database/sql` driver pattern.

See [DESIGN.md](DESIGN.md) for the full spec and the reasoning behind each
decision.

## Supported providers

| Provider          | Adapter         | Env var              |
|-------------------|-----------------|----------------------|
| OpenAI            | `openaicompat`  | `OPENAI_API_KEY`     |
| OpenRouter        | `openaicompat`  | `OPENROUTER_API_KEY` |
| X.AI (Grok)       | `openaicompat`  | `X_AI_API_KEY`       |
| Local (Ollama, …) | `openaicompat`  | — (keyless)          |
| Claude            | `anthropic`     | `ANTHROPIC_API_KEY`  |

Four of the five speak the OpenAI Chat Completions wire format, so a single
adapter covers them; Claude uses its native API (in a later build step).

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
go run ./cmd/playground
go run ./cmd/playground -prompt "your prompt here"
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

Memory lives in `.agent-memory.db` (pure-Go SQLite, gitignored — delete to
reset) and reuses `OPENROUTER_API_KEY`. It is best-effort: with no key the agent
runs statelessly. Each mneme call is bounded by a 30s timeout.

**Caveats (read before trusting recall):**

- **Self-correcting, but only where re-observed.** The agent runs mneme in its
  `Consolidate` strategy: each store reconciles the new facts against existing
  ones (ADD / UPDATE / DELETE), so a changed fact about a mutable repo *replaces*
  the stale one instead of piling up beside it — bump the Go version and a later
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
  exist silently degrades search (no error) — delete `.agent-memory.db` first.
- **Storing is on the happy path.** After the answer prints, the store does a
  synchronous extraction + embedding call before the run returns, so you see the
  answer and then wait briefly.

## Running untrusted code (sandbox backends)

Every effect the agent causes — running a command, reading/writing a file — flows
through one `sandbox.Sandbox` boundary (see `SANDBOX.md`). The backend, not the
tool, decides how strongly that boundary isolates:

| `-sandbox` / `-runtime` | isolation | use for |
|---|---|---|
| `local` *(default)* | none — host subprocess + path fence | code **we** wrote and trust |
| `docker` / `runc` | process — container, shared host kernel | isolated-but-not-hostile |
| `docker` / `runsc` | kernel — gVisor userspace kernel | **arbitrary, model-authored code** |

```sh
# Run the agent's `run`/`search` commands inside a locked-down container
# (network off, root fs read-only, CPU/memory/pids capped, non-root user):
go run ./cmd/agent -sandbox=docker -task "..."

# Treat the task's code as HOSTILE: require gVisor and refuse to start on
# anything weaker. Fails closed — `-untrusted` without `-runtime=runsc` will not
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
  read-only host `GOMODCACHE` mount — exposed via the library `docker.Options`
  (`ExtraMounts`), which the integration tests exercise.
- **The workspace is the only writable mount.** For a genuinely untrusted task,
  point the sandbox at a *throwaway copy*, not your live checkout — the backend
  takes a `dir`; the trust decision is the caller's.
- **The fence is symlink-safe.** A symlink planted inside the workspace by
  in-container code can't redirect a host-side read/write off-root (the
  confused-deputy guard in `sandbox/local`).
- `issue-bot` and the `eval` harness stay on `local` (trusted fixtures) — they
  don't pay container startup cost.

## Status

Build order (see DESIGN.md, decision 11). **Done:** core types + registry, the
`openaicompat` adapter (chat + `iter.Seq2` streaming), normalized errors,
end-to-end tested. **Next:** native `anthropic` adapter → tools (self-contained,
dual schema) → `Runner` (tool-exec loop) → comparison/fan-in harness.

## Develop

```sh
go test ./...        # deterministic unit tests (no network)
go run ./cmd/playground   # live round-trip (needs keys in .env)
```
