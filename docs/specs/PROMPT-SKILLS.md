# Plan: Prompt + Skills surface for the driver-os harness

## Context

driver-os is a Go agent harness (tool-calling loop over OpenRouter/Anthropic
providers) with a validated eval suite (`eval/`), a SWE-bench adapter, and
measured findings on model selection (cheap solver + flagship reviewer;
plan-stage A/B; review-gate). The harness's core loop, retries, streaming, and
TUI are mature and test-covered. The **instruction surface is not**: the native
tool-calling system prompt (`nativeSystemPrompt()`, agent/loop_tools.go) is four
sentences — "use tools, base actions on results, answer when done." Tool
descriptions are one line each. There are no per-model prompt variants. SKILL.md
support exists and was A/B-validated once (S1: base 0/2 vs skilled 2/2 at 9×
less cost), but no library of coding-discipline skills exists.

Research basis (2026-07-03 web sweep, 3 agents, all claims cited in session):

- Scaffold sensitivity is inversely correlated with model strength: scaffold
  swap alone moves SWE-bench Verified up to 15 pts for Kimi K2 vs ~nothing at
  the frontier (Epoch AI). Cheap models are exactly where prompt work pays.
- Interventions that supply EXTERNAL structure transfer to weak models
  (injected plans, real test execution, fixed pipelines, short prompts, simple
  edit formats); interventions asking the model to self-supervise backfire
  (self-critique −26% on weaker models; LLM-generated rules files −3% success
  +20% cost; JSON-mode −10-15%).
- Anthropic claims tool-description refinement alone drove SWE-bench SOTA for
  Sonnet 3.5. Our own run-tool cwd fix (stating cwd in the tool Desc stopped
  models assuming a chroot) validated the mechanism locally.
- Instruction budget: frontier thinking models follow ~150-200 instructions;
  cheap models fewer, and long/complex prompts measurably degrade them
  (IFScale; Haiku grading kappa 0.32 vs 0.54 under a complex prompt).
- IMPORTANT/NEVER emphasis measurably improves adherence and is most needed on
  weaker models (Claude Code's 2026 prompt dropped most of it as models
  improved; we target cheap models, so we keep it).
- Skills without per-skill evals can be a pure token tax (+22% tokens, zero
  output change in one community A/B). Anthropic's guidance: baseline first.

## Goal

Lift cheap-model (gemini-3-flash, deepseek-v4, glm-5) code quality on our eval
suite by investing in the instruction surface, in measured slices.

Secondary goal (clarified): transfer the same distilled content to Claude Code
run on cheaper Claude tiers. In our deployment, Fable 5 (Mythos-class) is the
top tier and Opus 4.8 sits below it at roughly 3-5× lower cost, with Sonnet 5
and Haiku 4.5 cheaper still. The intended comparison is: Claude Code + Opus (or
Sonnet) + our CLAUDE.md/skills content vs Claude Code + Fable with defaults.
This is a secondary, unmeasured-for-now goal; only the harness slices below
carry acceptance gates.

## Slices (in measurement order)

### Slice 1 — Fatten tool descriptions — ALREADY DONE (audited 2026-07-03)
Audit of agent/tools.go + agent/skill/tool.go found all 8 tools already carry
structured multi-sentence Desc/NativeDesc (when-to-use, cwd invariant, output
caps, append semantics, failure-mode hints) from prior dogfood iterations
(run-cwd fix, dep-browsing tool, write-append hint). The plan's "one line
each" premise was stale. No A/B needed; slice closed as pre-existing. The
ladder starts at Slice 2.

### Slice 2 — Structured native system prompt
Replace the 4-sentence `nativeSystemPrompt()` with a tight, sectioned prompt of
at most ~50 instructions, with IMPORTANT/NEVER emphasis. Explicitly NOT a port
of Claude Code's 12k tokens — short, because cheap models degrade under long
prompts. Rule content (final wording is part of the slice, these are the
calibrated forms):

- **Verification**: "Before answering, run the most relevant check available
  (tests, build, or executing the changed code). If no check is feasible —
  missing tests, broken deps, read-only or explanation-only task — say exactly
  why and what weaker verification you did instead." Scoped, not absolute: an
  explanation-only task must not trigger a test run, and long suites should be
  narrowed to the affected package.
- **Test integrity**: "NEVER weaken, delete, or special-case a test merely to
  make it pass. Adding regression tests, updating golden files, or changing
  tests to reflect intentionally changed behavior is normal work — do it and
  state the reason." Blocks reward hacking without blocking test maintenance.
- **Conventions**: mimic surrounding code style; use existing utilities.
- **Dependency paranoia**: never assume a library is available; check
  go.mod/imports first.
- **Comment policy**: no comments explaining changes; comment only non-obvious
  WHY.
- **Scope discipline**: do what was asked, nothing more; no drive-by refactors.

Also check token-cost delta, since the prompt is re-sent every turn.

### Slice 3 — Per-model prompt variants
opencode pattern: select prompt text by model family. Two variants + fallback,
not a matrix — and every primary target model routes intentionally:

- **persistence variant** (anti-quitting pressure: "keep going until solved;
  only stop when verified"): GPT-class, DeepSeek, **GLM**, and **Gemini** — all
  three named cheap targets (gemini-3-flash, deepseek-v4, glm-5) land here
  explicitly, not via fallback.
- **scope variant** (scope discipline instead): Claude-class.
- **terse fallback**: unknown models only.

The catalog tests (below) must assert gemini-3-flash, deepseek-v4-*, and glm-5
each route to the persistence variant; a primary target reaching the fallback
is a test failure. If dogfood later shows Gemini or GLM need different text
than GPT/DeepSeek, splitting the persistence family is a follow-up slice with
its own A/B, not a day-one guess.

**Routing is a first-class component, not string.Contains in the loop**: a
centralized model-family matcher (one table: pattern → family) that normalizes
provider prefixes, version suffixes, and OpenRouter decorations (`:free`,
`:nitro`, preview tags), with table-driven tests covering every model ID in our
catalog plus adversarial IDs (aliases, unknown vendors). Unknown IDs route to
the terse fallback and the choice is logged/observable in the run transcript so
a misroute is visible in dogfood, never silent.

### Slice 4 — Discipline skills library
2-3 skills in the existing SKILL.md system: tdd (red-green-refactor with hard
gates), systematic-debugging, verification-before-completion. Authoring rule:
write the eval FIRST (baseline without the skill on ≥3 scenarios), then write
minimal skill content to close observed gaps. Ship only skills that move the
eval; drop ones that don't (avoid the +22%-tokens-for-nothing failure).

## Measurement plan

**Task split (anti-overfitting).** Fixed before any authoring begins:
- **Tune set**: a subset of dogfood/selfhist tasks used while writing prompt
  text — iterate freely, no acceptance claims from it.
- **Holdout set**: the remaining selfhist tasks plus a small SWE-bench slice
  (reusing the R4a stride-sampling), including the harder tier where cheap
  models do NOT ceiling. Never used during authoring; acceptance runs only.
  The same holdout is reused across slices, under a **blinding protocol** so
  earlier acceptance runs can't steer later authoring. The acceptance verdict
  is computed **mechanically by the eval runner**, and until all four slices'
  prompt/skill text is frozen the author sees only the verdict plus blind
  aggregates: regression count, improvement count, total pass count, total
  iterations/tokens — **no task IDs, no per-task results, no transcripts**.
  The escalation rule below is also mechanical: the runner re-runs the flipped
  tasks itself without revealing which they were. Slice content is authored
  against the tune set only; any failure analysis a later slice needs happens
  on tune-set transcripts. After slice 4 freezes, per-task holdout results and
  transcripts open for the post-mortem writeup.

**Design (power at small N).** Paired per-task A/B: same task, same model
(gemini-3-flash coding baseline; deepseek-v4-flash as a second reader on the
final gate), same seed/temp-0 where the provider honors it, config differing
only in the slice under test. Binary pass/fail at N≥2/$5 cannot support a
pass-rate claim on its own, so:
- **Primary metric**: pass-rate on the holdout, evaluated directionally — a
  slice must show no task that regresses A→B across both trials, and the
  pass-rate point estimate must not drop.
- **Effect-size floor, decided up front**: a pass-rate *improvement* claim
  requires ≥2 holdout tasks flipping fail→pass consistently (both trials);
  a single task flip is recorded as noise, not signal.
- **Secondary metrics** (higher-powered at small N, near-deterministic):
  iterations, input/output tokens, wall-clock. Accept on these only if the
  primary shows no regression (see next point).
- **Token/iteration-only acceptance is gated on the non-ceilinged holdout**:
  a slice that saves work but shows any pass regression on the harder holdout
  tier is rejected — cheaper-but-dumber is the failure mode we're guarding.
- **Escalation rule**: if a slice looks positive but under-powered (exactly 1
  task flip), spend one extra confirmatory run (N+2 on the flipped tasks)
  before accepting — bounded re-test, not open-ended.

**Combined gate (interaction risk).** After individually accepted slices ship,
one all-on vs all-off run on the full holdout at the same N:
- Accept the combination only if it meets the same no-regression rule as a
  single slice.
- If all-on regresses, **bisect by slice order** (1+2, then +3, then +4) to
  attribute the interaction; the offending slice is reverted or trimmed — the
  default action is revert, with "trim and re-run one confirmatory A/B" as the
  only alternative. Slices are independent flags in config precisely so
  rollback is one switch, not a code revert.

**Budget**: ~$5/slice for the paired runs + up to ~$5 total for confirmatory
re-tests and the combined gate — ~$25-30 for the whole ladder, in line with
prior A/Bs (TRIAD-AB ~$4.50, R2-R3 ~$8).

## Risks / open questions

- Even with the holdout split, our task pool is small; a genuinely null slice
  can still sneak through as directional noise. Mitigation is the effect-size
  floor plus the combined gate, not more spend.
- Instruction-budget interaction between slices 2+3+4 is explicitly what the
  combined gate exists to catch.
- Skills triggering: our skill loading is explicit (flag/TUI toggle), so the
  community's undertriggering problem mostly doesn't apply yet, but will if we
  add auto-selection later.

## Task split — FROZEN 2026-07-03, before any slice authoring

Inventory: toy (calc, mathx), agentic (trace, repofix, selfhist-eviction,
selfhist-verifygate), designed-hard (task1 limiter/pool, task2 intervals,
task3 csvcut, fasthttp #2272), SWE-bench (by instance ID).

- **Tune set** (author against, iterate freely): calc, mathx, trace,
  selfhist-eviction.
- **Holdout set** (mechanical acceptance only; transcripts sealed until
  slice-4 freeze): selfhist-verifygate, repofix, task1, task2, task3,
  fasthttp, + 3 SWE-bench instances reused from the R4a stride sample
  (the non-ceilinged tier = task1/task2/fasthttp + SWE-bench, where the
  cheap baseline does NOT pass reliably).

Known-stats disclosure: published aggregate baselines for task3/fasthttp
(TRIAD-AB, review-gate §5.0) have been read by the author; the seal applies
to per-task A/B outcomes and transcripts from THIS plan's runs.
