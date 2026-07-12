# Changelog

All notable changes to this project are documented here. This project is
pre-1.0; the API and CLI surface may change between minor versions.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased] — 0.2.0: repository split

This repository was narrowed to the public core (docs/specs/REPO-SPLIT.md,
2026-07-12). The TUI, council, escalation ladder, eval suites, model-research
findings, and the `driver`/`driver-agent`/`chat` binaries moved to a private
lab repository; the GoBench mining/grading/launch machinery moved to a
separate `gobench` repository. Changes to those components are no longer
tracked here.

### Changed (breaking)
- **Commands**: `cmd/agent` (`driver-agent`), `cmd/driver`, `cmd/chat`,
  `cmd/council`, `cmd/eval`, `cmd/issue-bot`, `cmd/commit-msg`, and the other
  research entrypoints were removed from this repository. The public
  executables are now `cmd/runner` (reference headless runner) and
  `cmd/gobench-validate` (benchmark instance validator, preview).
- **Memory**: the long-term memory backend (mneme integration) moved out;
  `memory/` here is the interface contract only. The headless memory backend
  is an Extras capability supplied by the embedding application.
- **Docs**: findings, archive, and most research specs moved to the lab
  repository; this repo keeps the specs that govern its own surface.
- **Go API**: `bundle.Verify(path string)` — the `rerun bool` argument and
  `VerificationResult.RerunOutput` are gone (see the `-rerun-verify` removal
  below); embedders that passed `false` just drop the argument.

### Added
- `runner bundle verify <bundle-dir|manifest.json>`: offline proof-bundle
  verification (manifest hash, per-component digests, embedded-key
  signature).
- Proof bundle schema v2: separates reproducible evidence (patch, transcript,
  captured verifier output) from producer attestations; optional Ed25519
  self-signing via `DRIVER_BUNDLE_SIGNING_KEY`. v1 bundles still verify.

### Removed
- `runner bundle verify -rerun-verify`: it executed an untrusted `sh -c`
  command recorded in the bundle and could not reconstruct the original
  environment, so it was neither safe nor a real reproduction. Verification
  is now offline only and never executes bundle contents.
- The accidentally tracked root `runner` build artifact (now gitignored).

### Migration
- Scripted callers of `driver-agent`/`driver`: those binaries now build from
  the lab repository; their changelog lives there. Callers of the public
  surface should target `cmd/runner`, which keeps the same typed-outcome exit
  codes and `-format text|json|ndjson` contract.

## [0.1.0] - 2026-07-06

**Pre-split history.** This entry describes the monorepo as first released;
several components listed below (agent CLI, memory backend, council, eval
harness, TUI, dogfood integrations) have since moved out of this repository —
see the 0.2.0 notes above.

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
