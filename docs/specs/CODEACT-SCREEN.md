# CodeAct-lite screen — is code-as-action worth a build?

> **DECIDED 2026-07-06 — NO (do not default, do not build Stage 2).** Ran the full
> staged screen (docs/findings/PARALLEL-DISPATCH-AND-CODEACT-PROBE.md §3): a real
> `-codeact` knob (commit cd5fad5) A/B'd on greenfield + 3 internal edit cases + an
> 8-instance SWE-bench spine subset, cheap solver, solo. **Resolve: zero effect on
> every screen** (9/9=9/9 internal; 5/8=5/8 swebench, SAME instances) — confirms the
> verifier binds, not the action space. **Efficiency: robust only on greenfield**
> (−64% iters); a wash-to-worse on real editing. Verdict: keep `-codeact` as a shipped
> experimental opt-in (CLI only; TUI wiring NOT justified); narrow use = greenfield
> scaffolding batches. The staged design + kill-cheap discipline below worked as
> intended — Stage 0's greenfield win did NOT generalize, exactly as the drift/verifier
> priors predicted.

Status: DESIGN (superseded by the DECIDED banner above). Screen, not confirmatory.
Frozen spine: SWEBENCH-GATE-EXPERIMENT.md §2.1 stride-30 IDs (same 30 M0/M2 used).
Related: docs/findings/VERIFIER-ORACLE-WALL.md, docs/specs/NORTH-STAR.md,
docs/specs/M2-MEASUREMENT.md (screen template), docs/specs/SESSION.md.

## The question

The 2025–2026 agent-architecture literature has one pattern with direct,
replicated SWE-bench evidence: **CodeAct** — the agent's action *space is
executable code* (it writes a program that reads/edits/runs, the whole thing
executes, results feed back), rather than one structured tool call per turn.
OpenDevin/OpenHands CodeAct reported ~+17% relative over SWE-agent at the time,
and up to ~20% over JSON/text action spaces across 17 models.

driver-os today is NOT CodeAct. Actions are discrete structured tool calls
(`read_file`, `edit_file`, `run`, …). The agent *can* run arbitrary shell
through `run` (`sh -c`), but each action is still one call; it never composes a
multi-step program as a single action with fed-back results.

**Does giving driver-os a code-as-action space lift resolve on the frozen spine,
at ≤ equal cost, on the cheap substrate (gemini-3-flash)?**

## Prior: skeptical — design the screen to KILL cheaply

Two driver-os-specific facts push against a lift here, and the literature adds a
third:

1. **The verifier binds, not the solver (VERIFIER-ORACLE-WALL).** M0 measured
   frontier FP rate 23.3% CI [9.9, 42.3]; the perfect-router ceiling (~24/30)
   sits barely above the cheap substrate. Resolve is gated by *knowing when a
   fix is right*, not by how the solver expresses its actions. CodeAct changes
   the action space, not the oracle — so the prior says it moves cost/latency,
   maybe, and resolve little.
2. **Cheap substrate amplifies the CodeAct risk.** The literature's own caveat:
   weaker models write *worse* code-actions, so a code-as-action space can widen
   the capability gap it is meant to close. On a gemini-3-flash substrate this
   can go the wrong way. driver-os's own `edit_file` is deliberately
   anchor-based (not line-numbered) precisely because line/offset editing drifts
   when a model batches edits (HP-7) — a shell-script "code action" that does
   multi-file edits via `sed`/heredoc REINTRODUCES exactly the drift edit_file
   was built to remove. So a naive code-as-action arm may *regress* on
   multi-edit instances.
3. **The compute confound (the single most important 2025–26 methodological
   finding).** Most "pattern X beats ReAct" wins evaporate once test-time tokens
   are normalized. Any resolve delta here is meaningless unless read at equal
   cost. **This screen compares cost-normalized, not raw resolve.**

Given all three, the screen's job is to get cheap signal and KILL the idea if the
prior holds — only a real signal justifies the Stage-1 build.

## Staged design — cheapest kill gate first

> **Stage 0 micro-probe RUN 2026-07-06** (docs/findings/PARALLEL-DISPATCH-AND-CODEACT-PROBE.md
> §2): 2 greenfield Go tasks × {baseline, code-as-action steer} × 1 trial, cheap solver.
> All 4 passed; the steer cut iterations −64% and cost −55%. DIRECTIONAL ONLY — n=2, no
> CI, and greenfield file-creation is the shape MOST favorable to CodeAct (one heredoc vs
> N write_files) and least like edit-in-large-repo SWE-bench. Efficiency signal, not
> resolve. Verdict: promising-enough-to-not-kill → the real decision still needs the
> Stage-1 spine screen below with resolve as the metric.

### Stage 0 — prompt-only code-as-action probe (do this first; ~1 arm)

driver-os already ships a capable code executor: `run` (`sh -c`, pipes, `&&`,
heredocs). Stage 0 needs **no new tool code** — it is a solver-prompt variant
that steers the model toward code-as-action using the existing `run` tool:
compose each sub-goal as one shell *script* (`run` with a heredoc / `&&` chain
that reads, transforms, writes, and tests in one action) instead of a sequence
of discrete file-tool calls. Everything else — gates, verify, spine, model —
held identical to the M2 arm-S baseline.

- **Arm B (baseline):** current prompt, discrete tool calls. gemini-3-flash solo.
- **Arm C (code-act-lite):** same everything + the code-as-action prompt steer.
- Paired, same 30 instances, `-n=1`, same caps as `m2-screen.sh COMMON`.

This is a *weak proxy* for full CodeAct (shell scripts, no persistent
interpreter, no rich stdlib, no fed-back program state). State that honestly:
- A **null in Stage 0 is "no cheap signal + the drift risk is real"**, not
  "CodeAct is definitively dead" — the proxy under-powers the idea.
- A **positive signal in Stage 0 is a strong go** — if even a shell-script proxy
  on a cheap model lifts cost-normalized resolve, the real interpreter is very
  likely to.

Cost: ~$1–5 (one extra cheap-solo arm vs the existing M2 arm-S numbers, which
serve as Arm B if the pins still match; re-run B if git/pricing SHAs drifted).

### Stage 1 — real CodeAct tool (ONLY if Stage 0 signals)

Build a persistent-interpreter action space and re-measure. driver-os already has
the substrate: the stateful `Session` capability (SESSION.md — `Session.Exec` +
ProcessHost) supports a long-lived interpreter process. Stage 1:

- Add a `python` (or `code`) tool backed by a persistent Session interpreter, so
  program state (imported modules, computed values, open file handles) survives
  across turns — the actual CodeAct mechanism, not a shell proxy.
- Expose the file ops as callable helpers inside that interpreter (a thin
  `read(path)`, `edit(path, old, new)`, `run(cmd)` shim over the existing tool
  cores in `agent/tools.go`) so the model orchestrates them *programmatically*
  and keeps `edit_file`'s drift-free anchoring.
- Re-run the paired screen (Arm C' = interpreter CodeAct) vs Arm B.

Stage 1 is a real build (new tool, session wiring, prompt) — justified only by a
Stage-0 signal, never speculatively.

## Metrics & decision rule (carry-the-n)

Mirror M2-MEASUREMENT.md exactly — this is a screen, n=30 CANNOT confirm a small
non-inferiority bar:

- **Primary:** resolve k/30 with 95% CI via `eval/scripts/stats_block.py`, per
  arm, paired same-instance.
- **Cost:** $/resolve from `eval/pricing.go`, per arm. The comparison of record
  is **cost-normalized resolve** — Arm C must not simply spend more tokens to
  win (the compute confound). If Arm C resolves more only because it burned more
  tokens, that is a NULL, not a win.
- **Paired deltas:** count instances resolved by C-not-B and B-not-C (McNemar
  shape), not just the marginal rates — a wash of equal magnitude in both
  directions is the drift-vs-batching tradeoff, and it is a KILL.
- **Verdict (screen-grade, like M2):** KILL / PROMISING / MURKY, plus the cost
  signal, plus a statement of the MDE. n=30 detects only large effects
  (roughly ≥ ~20pp shifts with any confidence); a confirmatory run is the whole
  SWE-bench Lite pool (~300, ~$200+/arm) and is out of scope for the screen.

### Kill criteria (Stage 0)

KILL the CodeAct direction (do not build Stage 1) if ANY hold:
- cost-normalized resolve delta (C − B) ≤ 0, OR
- C wins on raw resolve but only by spending materially more $/resolve (confound),
  OR
- the paired B-not-C count is non-trivial and concentrated on multi-edit
  instances (the predicted drift regression) — i.e. code-as-action bought
  batching but paid it back in edit drift.

PROMISING (build Stage 1) only if C shows a cost-normalized resolve lift with the
paired delta pointing the same way and no drift-regression cluster.

## Harness

Clone `eval/scripts/m2-screen.sh` to `eval/scripts/codeact-screen.sh`: same
`IDS`, same `COMMON`, two arms (B, C) driven through `go run ./cmd/eval
-case=swebench`. The only difference between arms is the solver prompt variant
(Stage 0) or the tool set + prompt (Stage 1). Record git + `eval/pricing.go`
SHAs in the run manifests, as M2 does. Reuse M0/M2 arm-S as Arm B iff the pins
match; otherwise re-run B in the same batch so the pairing is clean.

## Open questions before running

- Where does the code-as-action prompt steer live? A prompt variant needs a
  knob to select it per run without forking `buildSystemPrompt` — confirm the
  cleanest injection point (a `Config` field vs an eval-only prompt override).
- Does the M2 arm-S run still match current pins, or must Arm B be re-run?
- Interpreter choice for Stage 1: Python (richest CodeAct prior art) vs a Go
  eval loop (native, but weaker library ecosystem for the model to lean on).
