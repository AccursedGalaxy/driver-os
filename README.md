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
