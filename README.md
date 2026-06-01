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

- **Additive, never corrected.** mneme v1 has no update/delete pass — facts
  *accumulate*. A fact about a mutable repo goes stale and is never overwritten:
  bump the Go version and the store will hold *both* values, and may recall both.
  Recalled facts are labelled possibly-stale in the prompt so the model verifies
  with tools (Principle 4), but that is the only guardrail. Treat memory as a
  hint, not a source of truth.
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

## Status

Build order (see DESIGN.md, decision 11). **Done:** core types + registry, the
`openaicompat` adapter (non-streaming chat), normalized errors, end-to-end
tested. **Next:** streaming (`iter.Seq2`) → native `anthropic` adapter → tools
(self-contained, dual schema) → `Runner` (tool-exec loop) → comparison/fan-in
harness.

## Develop

```sh
go test ./...        # deterministic unit tests (no network)
go run ./cmd/playground   # live round-trip (needs keys in .env)
```
