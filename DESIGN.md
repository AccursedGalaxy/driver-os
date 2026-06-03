# driver-os — LLM provider abstraction

A small Go library for experimenting with LLM **model behavior** across providers.
One uniform interface; swap providers freely, or run several at once (compare /
race them). Modeled on the `database/sql` driver pattern: a stable core + pluggable
provider adapters.

## Targets

| Target            | Protocol            | Adapter              | Base URL (default)            | Env var              |
|-------------------|---------------------|----------------------|-------------------------------|----------------------|
| OpenRouter        | OpenAI-compatible   | `openaicompat`       | `https://openrouter.ai/api/v1`| `OPENROUTER_API_KEY` |
| X.AI (Grok)       | OpenAI-compatible   | `openaicompat`       | `https://api.x.ai/v1`         | `X_AI_API_KEY`       |
| OpenAI            | OpenAI-compatible   | `openaicompat`       | `https://api.openai.com/v1`   | `OPENAI_API_KEY`     |
| Local (Ollama, …) | OpenAI-compatible   | `openaicompat`       | `http://localhost:11434/v1`   | (none / `ollama`)    |
| Claude            | Anthropic Messages  | `anthropic` (native) | `https://api.anthropic.com`   | `ANTHROPIC_API_KEY`  |

**Key insight:** four of five targets speak the OpenAI Chat Completions wire format,
so a *single* OpenAI-compatible adapter (parameterized by base URL + key + model)
covers them. Only Claude needs its own adapter — and we use Claude's **native** API
(not Anthropic's OpenAI-compat shim) to keep extended thinking, prompt caching,
structured outputs, and strict tool schemas.

## Design decisions

| # | Decision        | Choice |
|---|-----------------|--------|
| 1 | Foundation      | Wrap the **official SDKs** (`openai-go`, `anthropic-sdk-go`). Goal is model behavior, not protocol internals; the SDKs give us streaming, retry/backoff, and tool plumbing for free. |
| 2 | Interface shape | **Small core `Provider`** (chat + stream) that every provider implements, plus **optional capability interfaces** (`Embedder`, …) discovered via type-assertion. `Capabilities()` advertises support. Idiomatic Go (`io.Reader` + `io.ReaderAt`). |
| 3 | Content model   | A message's content is a **`[]ContentPart`** (typed: text, image, tool-call, tool-result), with **text-first helpers** (`llm.User("…")`) so the common case stays a one-liner. Vision-ready without being built now. |
| 4 | Tools           | **Self-contained** `Tool{Name, Description, Schema, Handler}`. **Dual schema**: raw `json.RawMessage` *or* reflected from a Go struct. Tool **execution lives in a separate `Runner`**, never in the `Provider` interface — adapters only surface tool-calls. |
| 5 | Streaming       | Core primitive is **`iter.Seq2[Chunk, error]`** (range-over-func). A **channel `merge` adapter** is provided for fan-in / racing multiple live streams. |
| 6 | Provider extras | **Hybrid.** Broadly-shared sampling params (`Temperature`, `MaxTokens`, `TopP`, `Stop`, `Seed`) are typed fields on `Request`. Provider-specific knobs go through a **passthrough map** (`ProviderOptions`), with **typed helpers** per provider for discoverability (e.g. `anthropic.Thinking(2048)`). Adapters **ignore** options they don't understand (preserves swap-ability). |
| 7 | Config          | **Env** (`.env`) for secrets + a **programmatic `Registry`** with **preset constructors**. No config file until a model catalog demands it. |
| 8 | Response        | **Rich**: content, tool-calls, normalized `FinishReason`, `Usage` (prompt/completion/total + cached + **reasoning**, the last a subset of completion — billed inside it, broken out for visibility on thinking models), resolved `Model`, and a **`Raw any`** escape hatch to the underlying SDK response. Latency measured one layer up (the agent loop records per-turn `ModelMs`/`ToolMs` on each `Step`). Streams collapsible to a full `Response` via a collector. |
| 9 | Errors          | **Lean normalized taxonomy**: `*ProviderError{Provider, Kind, Retryable, StatusCode, err}` wrapping the original (so `errors.Is/As` work and `Raw` stays reachable). Kinds: `RateLimit`, `Auth`, `ContextLength`, `ContentFilter`, `Canceled`, `Unsupported`, `Unknown`. SDKs already handle retry/backoff; we only classify so callers can react uniformly. |
| 10| Layout          | See below. **`llm/` core has zero SDK dependencies.** Each adapter package pulls only its own SDK. |
| 11| Build order     | **Thin vertical slice first**, then grow. |
| 12| Run transcript  | Every agent run is **addressable by a stable, time-sortable `RunResult.ID`** (the spine), with wall-clock bounds (`StartedAt`/`EndedAt`). The loop stays pure I/O-free; persistence lives at the edges via `agent.RunRecord` + `WriteTranscript`, which the CLI calls **by default** — writing `<id>.json` (full trace) + appending a trace-less summary to `runs.jsonl` under `$XDG_STATE_HOME/driver-os/runs/`. eval `Trial`, council `AgentTrace`, and the commit-msg dogfood record all reference the same run by ID instead of re-embedding their own copy. |

## Package layout

```
driver-os/
  go.mod                    module github.com/AccursedGalaxy/driver-os
  llm/                      CORE — Provider interface, Request/Response/Message/
                            ContentPart/Tool, Registry, errors, capabilities.
                            NO SDK deps.
  provider/
    openaicompat/           OpenAI-compatible adapter (openai-go). Exposes
                            New(Config) + presets OpenAI/XAI/OpenRouter/Ollama.
    anthropic/              native Claude adapter (anthropic-sdk-go).
  runner/                   tool-exec loop + model-comparison / fan-in harness.
  cmd/playground/           experiment entrypoint (main.go).
  examples/
```

## Core interface (target shape)

```go
type Provider interface {
    Name() string
    Capabilities() Capabilities
    Generate(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request) iter.Seq2[Chunk, error]
}

// optional, promoted via type-assertion:
type Embedder interface { Embed(ctx, EmbedRequest) (*EmbedResponse, error) }
```

System-prompt placement (top-level for Claude, message-role for OpenAI), exact
`Chunk` shape (text delta + tool-call deltas + terminal usage), and `.env` loading
are implementation details resolved inside the adapters.

## Build order (decision 11 = A, thin vertical slice) — ✅ complete

All six original slices shipped and are end-to-end tested:

1. ✅ `llm` core types + `Registry`; `openaicompat` chat (non-streaming);
   `cmd/playground` proving a real round-trip against OpenRouter and X.AI.
2. ✅ Streaming (`iter.Seq2`) on `openaicompat`.
3. ✅ `anthropic` native adapter.
4. ✅ Tools (self-contained, dual schema) surfaced in responses.
5. ✅ `Runner` — tool-exec loop.
6. ✅ Comparison / fan-in harness (merge adapter, race/compare N providers).

Work since then has built an agent-harness research platform on this core —
the agent loop with cross-run memory, the sandbox isolation tiers
(`SANDBOX.md`, `SESSION.md`), the eval + dogfood harness (`DOGFOOD.md`), and
the council adversarial-review feature (`COUNCIL.md`). The open research
backlog is tracked in `HARD-PROBLEMS.md`.
```
