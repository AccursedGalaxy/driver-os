# REVIEW-GATE — the solve-cheap / review-grounded closing gate

Status: DESIGN (researched 2026-07-01, not yet built). Companion to the
closing gates in `agent/agent.go` (`verifyTermination` / `upgradeIfVerified`)
and to `council/critic_code.go`. Motivated by the fasthttp#2272 four-way probe:
all four models (flagship → cheap) passed every automated gate, but the cheap
tier shipped defects only a *reviewer* could see (a goroutine/connection leak;
an unguarded code path the tests don't exercise).

## The problem, precisely

The harness's acceptance signal today is execution-only (`VerifyCmd`). The
probe showed execution gates saturate before models do: deepseek-v4-flash
passed the full fasthttp suite + `-race` with a patch that deletes stop
semantics (eternal reconnect + goroutine leak); gemini-3-flash passed while
guarding only the tested path. Expect this generally: ~8–31% of "plausible"
(test-passing) patches are wrong on SWE-bench-class tasks (PatchDiff 7.8%
provably incorrect; SWE-Bench+ 31% suspicious; METR measured 20–30%
false-green on hard tasks — matching our own ~23% best-of-N false-positive
wall).

## What the research says (three-stream survey, 2026-07-01)

Full notes in the session transcript; the load-bearing findings:

1. **Execution evidence outranks any judge.** Agentless ablation: majority
   vote 25.7% → +regression tests 27.0% → +reproduction tests 32.0%. AlphaCode:
   example tests kill >99% of samples. The judge rules only on what execution
   can't decide.
2. **The reviewer must be grounded and independent — not necessarily
   stronger or expensive.** Prompted same-model self-review is the one
   arrangement shown to be useless or harmful (Huang et al. ICLR'24:
   GPT-4 GSM8K 95.5→89.0 after self-correction; SELF-[IN]CORRECT; Greptile
   measured self-scoring of own findings as "nearly random"). Cross-model
   review helps (Olausson ICLR'24); a *smaller but critique-trained or
   thinking-mode* model works (OpenHands 4B critic: best-of-8 73.8% vs 57.9%
   random; CodeJudgeBench: Qwen3-8B-thinking beats 70B non-thinking judges).
3. **Reviewer false positives are the #1 product risk.** Greptile baseline:
   79% of comments were nits, 19% address rate. CriticGPT: model critics beat
   humans on recall but hallucinate/nitpick far more. Asking a judge to also
   explain-and-repair collapses verification accuracy 52.4%→11.0%
   (arXiv:2508.12358). Mitigations with measured effect: suppress-lists +
   explicit empty-verdict path (+50% acceptance, Qodo), numeric confidence
   hard gate (Anthropic security-review: ≥8/10), verbatim-quote re-grounding
   (PR-Agent: quote must match the diff or score 0), neutral framing.
4. **Executable refutation is the only hallucination filter with numbers.**
   Generated fail-to-pass tests as an acceptance filter: precision 60.8%→91.9%
   (Otter). A reviewer *claim* escalated into a runnable test settled by the
   sandbox is worth more than any verbal verdict. Build the reviewer as a
   test-proposer whose claims the sandbox settles, not as an oracle.
5. **Bound the loop; stop on external signals.** Refine-loop gains concentrate
   in round 1 with documented diminishing returns (Self-Refine); "iterate until
   reviewer is happy" without external stop signals actively flips correct
   patches to wrong (Huang). Production caps: OpenHands max_iterations 3,
   Claude Code stop-hook cap 8. Default: 2 review rounds.
6. **Harden the deterministic gate first — it's nearly free.**
   - `goleak`-style goroutine/resource assertions would have caught the
     deepseek leak deterministically; `-race` provably cannot (a leak is not
     a race).
   - Test-file immutability must be enforced by the HARNESS, not the prompt
     (typia incident: agent deleted 70% of the suite, "CI was green"; Claude
     3.7 card lists "unexpected modifications to test files" as a hack
     signature; anti-hack prompting alone: 70–95% of o3 hacks survive it).
   - Patch-region coverage + mutation-survivor checks convert "suite passed"
     into "suite passed AND the suite is adequate for this diff" (STING: 77%
     of SWE-bench Verified instances have ≥1 surviving mutant; SWE-ABS:
     ~$5/instance with cheap models).
7. **Review at the artifact boundary, not mid-trajectory.** Every production
   system (OpenHands FinishAction, Jules pre-diff, Devin/Copilot/Anthropic on
   PR) triggers on a completed patch. For judging *agent runs* specifically,
   the trajectory is a better signal than the diff alone (OpenHands: AUC 0.69
   code-survival label vs 0.45 benchmark-trained) — we HAVE the transcript;
   feed it.
8. **Calibrate on our own telemetry.** Greptile's single biggest win (address
   rate 19%→55%) came from filtering candidate findings against a memory of
   past human-rejected findings — not from prompting. We already have the
   dogfood-corpus culture (`record`, corpus-regression suite); review verdicts
   must be recorded from day one.

## Design

One new closing-gate stage plus deterministic hardening, mapped onto existing
seams. The reviewer NEVER replaces execution; it runs strictly after
`VerifyCmd` passes.

```
finish signal (answer / FinishTool / cap+upgrade path)
  └─ Stage 0  deterministic substance checks     (no model, ~free)
  └─ Stage 1  VerifyCmd                          (existing, authoritative)
  └─ Stage 2  Review gate                        (new, only if 0+1 green)
        reviewer = read-only grounded agent over the DIFF
        blocking findings → fed back to solver (VerifyContinue-style)
        bounded: ReviewRounds (default 2), then terminal
```

### Stage 0 — diff-substance checks (deterministic, before any model)

- **Test fence**: configured glob (default `*_test.go`, `testdata/`) is
  read-only to the solver — enforced in the sandbox/tool layer (like the
  symlink fence), not the prompt. A `run`-command mutation of fenced paths is
  detected post-hoc (`git status` on fenced globs) and downgrades the run to
  Unverified with a named reason. Also fences CI configs (`.github/`,
  `Makefile` opt-in).
- **Hack-signature scan** (cheap, harness-side): new `t.Skip`/build-tag
  exclusions, deleted assertions, "special case for test" comments. Signals,
  not verdicts: they lower the bar for Stage 2 blocking (and are surfaced to
  the reviewer).
- Later (separate slice): optional leak/coverage adjuncts — goroutine-count
  assertion around VerifyCmd for Go, patch-region coverage, mutation-survivor
  probe.

### Stage 2 — the reviewer

**Shape**: reuse `council.RunCriticCode`'s architecture — a READ-ONLY agent
(list_dir/read_file/search/go_doc) with a strict budget, pointed at the repo,
whose artifact is the solver's diff (`git diff` vs the run's base) + the task
text + (optionally) a compacted trajectory summary of the solver's run.
Agentic retrieval from the diff outward is the industry-converged design
(Cursor's agentic rebuild moved resolution 52%→70%).

**Independence policy**: `ReviewModel` must differ from the solver model
(harness-warns if same family); the reviewer is never told what produced the
patch; solver prose/self-praise is stripped — it judges task + diff + code.

**Prompt doctrine** (each element evidence-backed, see findings 3–4):
- Neutral framing: "Does this change correctly and completely accomplish the
  task without regressions?" — NOT "critique this" / "find N issues".
- An explicit suppress-list (style, naming, docs, theoretical perf, hypothetical
  refactors, anything in unchanged code unless the diff breaks it).
- An explicit empty-verdict path: "an empty findings list is a correct answer".
- Only two severities: `blocker` (wrong behavior, regression, resource leak,
  unhandled case the task requires) and `note` (recorded, never blocks).

**Structured verdict** (JSON, schema-validated like the council objection
array):

```json
{"findings": [{
   "file": "client.go",
   "quote": "<verbatim post-patch code>",
   "severity": "blocker|note",
   "confidence": 0-10,
   "failure_scenario": "concrete inputs/state → wrong outcome",
   "repro_cmd": "optional: a runnable command/test expected to FAIL on this code"
}]}
```

Harness-side validation (the anti-hallucination layer, all deterministic):
- `quote` must appear verbatim in the patched file — mismatch drops the
  finding (PR-Agent's score-0 rule).
- A finding blocks only if `severity=blocker` AND `confidence >= 8`.
- **Executable escalation**: if `repro_cmd` is present, run it in the verify
  sandbox. Confirmed-failing → hard block regardless of confidence;
  passing → the finding is downgraded to `note` (the reviewer's claim was
  refuted by execution). This is the centerpiece: the reviewer is a
  test-proposer; the sandbox is the judge of the judge.

**Repair loop**: blocking findings are fed back to the solver as an
observation (exactly the VerifyContinue pattern):
`[review] a reviewer found a blocking defect: <scenario> (<file>: <quote>).
Fix it without breaking the verified tests.` Then the FULL gate re-runs
(Stage 0 → 1 → 2). `ReviewRounds` caps the cycles (default 2). Exit
conditions, in order: no blocking findings → Answered; rounds exhausted with
blockers → Unverified (exit 2) with the findings in `RunResult.Review`.
No new outcome/exit code: the CLI contract stays stable; the review detail
lives in the result JSON.

### Config / API surface

```go
// Config additions
ReviewModel   llm.Provider  // nil = gate off (default)
ReviewRounds  int           // default 2
ReviewBudget  int           // reviewer read-budget (council-style)
TestFence     []string      // default ["*_test.go", "testdata/**"]; nil = off? (opt-out explicit)
```

CLI: `-review-model <id>` (off when empty), `-review-rounds`, `-test-fence`.

### Observability (TUI-compatible by construction)

No UI work here; everything flows through the existing seams:
- `Observer.Note` lines for gate progress ("review: round 1/2 — 2 findings,
  1 blocking (confirmed by repro)").
- A NEW optional `ReviewObserver` interface (type-asserted like
  `DeltaObserver`) with typed events (`ReviewStarted/Finding/Verdict`) — the
  TUI session can implement it to render a review panel; ndjson gains
  matching event types; nothing existing changes.
- Transcript: the reviewer's sub-run is recorded (its own agent transcript +
  verdicts in `runs.jsonl`), feeding the corpus-regression suite. Every
  finding's fate (repaired / refuted-by-execution / expired-at-cap) is
  labeled — this is the calibration telemetry (finding 8).

### What we deliberately do NOT build (evidence against)

- Same-model/self review as default (useless-to-harmful; measured).
- Scalar 1–10 "quality scores" as the verdict (intra-judge reliability below
  usable thresholds — Rating Roulette).
- A combined "review and fix it yourself" reviewer (52.4%→11.0% collapse;
  also breaks solver-context continuity — Cognition's argument).
- Unbounded reviewer↔solver loops (correct→incorrect flips).
- Mid-trajectory critics (nobody ships them; artifact boundary only).
- Training/tuning the solver against reviewer verdicts (obfuscated hacking;
  monitor recall → ~0 — OpenAI/METR, independently).

### Slice order

1. **Slice 0 — test fence + substance checks.** Deterministic, no model,
   closes the "edit the tests" hole the probe task text only *asked* models
   not to exploit. Small; testable with existing fixture patterns.
2. **Slice 1 — the review gate.** Single-pass reviewer after VerifyCmd,
   structured verdict + harness validation + repair loop (2 rounds) +
   Observer/ndjson events + result JSON. Reuses council critic machinery.
3. **Slice 2 — executable escalation** (`repro_cmd` arbitration). The
   differentiator; measurable on the banked fasthttp#2272 case (must catch
   the deepseek leak + the gemini Do() miss).
4. **Slice 3 — eval integration.** Add review-gate variants to the eval
   harness; score catch-rate / FP-rate / cost-delta on: fasthttp#2272 (both
   cheap-model diffs preserved), the three designed fixtures, and SWE-bench
   stride. Success = catches both known-bad diffs with 0 false blocks on the
   known-good flagship diffs.
5. **Later**: 3-vote majority on split verdicts, plan-stage critic (Jules:
   −9.5% failures), goleak/coverage/mutation adjuncts, differential testing
   between best-of-N candidates (attacks the N≥3 false-positive wall with
   execution, not diff-consensus — which we measured ANTI-selects).

### Open questions (for the grill/council)

- Reviewer sandbox: read-only tools over the LIVE workspace vs a clean export?
  (Council uses the live repo read-only; a repro_cmd needs execute rights —
  use the verify sandbox for execution, read-only tools for exploration?)
- Should the trajectory summary go to the reviewer in slice 1, or is diff+task
  enough until we measure? (OpenHands evidence says trajectory helps, but it
  was a TRAINED critic; token cost is real.)
- Test-fence default-on vs opt-in for `-review` runs only? Default-on changes
  behavior for existing users who legitimately ask the agent to write tests —
  probably fence only when the task/gate declares it (like -untrusted).
- Interaction with JARVIS-TEAM's Verifier role: same brief/machinery, or does
  the team Verifier simply CALL this gate? (Prefer: one implementation, two
  entry points.)
