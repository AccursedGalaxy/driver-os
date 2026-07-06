# Changelog

All notable changes to this project are documented here. This project is
pre-1.0; the API and CLI surface may change between minor versions.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## [0.1.0] - 2026-07-06

Initial public beta. The full history predates this tag; this entry summarizes
the surface as it stands at first release.

### Core library (`llm/`)
- Uniform multi-provider driver modeled on the `database/sql` pattern; swap
  providers behind one interface, or run several at once to compare or race them.
- `openaicompat` adapter covering OpenAI, OpenRouter, X.AI (Grok), and local
  OpenAI-compatible servers (Ollama).
- Native `anthropic` adapter for Claude: signed-thinking replay, the 5-family
  effort knob, and prompt caching.
- Registry, streaming (`iter.Seq2`), dual-schema self-contained tools, a runner
  tool-exec loop, and a comparison/fan-in harness.

### Agent
- `cmd/agent`: think→act→observe loop over the sandbox tools, with a
  scriptable Unix CLI (text/json/ndjson output, meaningful exit codes).
- Cross-run long-term memory via [mneme](https://github.com/AccursedGalaxy/mneme),
  stored only for tool-verified answers and reconciled on write.
- Review/verify gates and a model escalation ladder.

### Sandbox
- One effect boundary with three isolation tiers: `local` (host + path fence),
  `docker`/`runc` (container), and `docker`/`runsc` (gVisor, for untrusted
  model-authored code). Network off by default; symlink-safe fence.
- Long-lived process host and stateful shell sessions.

### Tooling & research
- Eval/dogfood harness (`eval/`) including a corpus regression scored against
  real human verdicts.
- Council: adversarial multi-model review (author ↔ critic ↔ referee) plus a
  consult/Q&A mode.
- Dogfood integrations: `cmd/commit-msg`, `cmd/issue-bot`.
- A terminal UI (`cmd/driver`).

[0.1.0]: https://github.com/AccursedGalaxy/driver-os/releases/tag/v0.1.0
