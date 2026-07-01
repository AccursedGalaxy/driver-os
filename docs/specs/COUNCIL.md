# Council — direction note

Status: **Slices 1–4 SHIPPED (2026-06-03).** Slice 4 = code mode (a read-only agent
critic over a repo); see build order item 4 for the surface and the two known gaps.
It was built by councilling its own design to converged (3 rounds) and then live
code-mode-dogfooded on its own diff — the code critic found and fixed 8 real bugs in
itself across 3 review rounds (3/3 answered once the answer-forcer landed). Earlier:
**Slices 1–3.** Slice 1: `council/record` + `council` +
`cmd/council` plan mode (new/critic/respond/status/finish), full recording,
structured-objection helper. Slice 2: round-N critic RE-VALIDATION (`revalidate`) +
the exception-only referee — councils now actually CONVERGE (the critic closes
high/medium objections, the referee breaks author↔critic disputes). Slice 3: the
host-wide `/council` skill (`~/.claude/skills/council/SKILL.md`) drives the loop in
any session, with `make install-council` putting the binary on PATH and an egress
confirmation before the first critic call. Slices 4–5 designed below. Grounded in real
runs + dogfooded on itself (see the dogfood findings and "Slice 2 convergence evidence").
This note records the decided design so we don't relitigate it. The council is a
**separate feature that consumes the agent harness as a grounding brick** — it
imports `agent`/`llm`/`sandbox` and changes none of them. The goal: any Claude
Code session on this host can run `/council` to have its plan, diff, or decision
adversarially reviewed by a second frontier model, with **every run fully
recorded** as a dogfood corpus for the harness.

It touches the research backlog at HP-3 (grounding), HP-5 (verification), and
HP-11 (the eval harness, which will ingest the corpus).

## Resuming this work (start here)

If you're a new session picking this up ("let's continue working on the council"),
this is the orientation.

**State (2026-06-03):** slices 1–3 are shipped (`2414a36` slice 1, `9695c5a` slice 2,
slice 3 = the skill + `make install-council` + `SkillVersion: "slice3"`). **Plan mode
works end-to-end and converges** — critic raises objections, the author (the session)
revises, the critic re-validates and closes high/medium objections, the exception-only
referee breaks disputes. With slice 3 you no longer drive the CLI by hand: run
`/council` in any session and the skill drives the loop. Slice 4 added **code mode**
(read-only agent critic over a repo) — see build order item 4. The "reviewed" finish
outcome + index hygiene (`council reindex`) gap is now CLOSED (2026-06-03); the
remaining code-mode task is **repo-grounded `revalidate`** (the last known gap), and
then **slice 5 (eval ingestion)**.

**Run it (slice 3):** `/council [file]` in any Claude Code session. The skill lives at
`~/.claude/skills/council/SKILL.md`; the `council` binary must be on PATH (run
`make install-council` from this repo once — installs to `~/.local/bin`). The skill
captures the plan/decision to a scratch file, shows an egress confirmation, then loops
`critic → (you revise + respond) → revalidate` until converged or the round cap.

**Drive a council by hand right now (plan mode).** The CLI is the backend; the
author is *you*, the session. Needs `OPENROUTER_API_KEY` in env (or `.env`).

```sh
# 1. Write the artifact (plan/decision) to a file, then create the run:
go run ./cmd/council new --mode plan --artifact PLAN.md --round-cap 3   # prints {run_id}
# 2. Critic raises objections (JSON to stdout):
go run ./cmd/council critic --run <id>
# 3. You revise PLAN.md to address them, write responses.json (shape below), then:
go run ./cmd/council respond --run <id> --from responses.json --artifact PLAN.revised.md
# 4. Critic re-reads the revision + referee adjudicates disputes:
go run ./cmd/council revalidate --run <id>        # JSON: verdicts_closed/stands, referee, convergence
# 5. Repeat 3–4 until "convergence":"converged" (or the round cap), then:
go run ./cmd/council finish --run <id>
go run ./cmd/council status --run <id>            # inspect any time
```

`responses.json` is `[{ "objection_id":"O1", "action":"claims-resolved"|"rejects-with-reason",
"text":"what you changed / why you decline" }]`. Only the critic can mark an objection
`resolved`; the author can only claim-resolve or reject-with-reason.

**Where the records are:** `$XDG_DATA_HOME/driver-os/council/` (default
`~/.local/share/driver-os/council/`; override `$DRIVER_OS_COUNCIL_DIR`). One line per
finished run in `index.jsonl`; full verbatim history in `runs/<id>/events.jsonl`. Query
the corpus with `jq`.

**Next task — slice 3 (host-wide `/council` skill).** Goal: any Claude Code session on
this host runs `/council` and the skill drives the loop above. Decisions already locked:
the author is the session (no `provider/anthropic`, so orchestration lives in the skill,
not a standalone binary); plan/decision mode first (code mode is slice 4); show an
**egress confirmation** of what will leave the session to OpenRouter before the first
critic call. First concrete steps:
1. Add a `make install` (or `go install ./cmd/council`) so the `council` binary is on
   `PATH` for any session.
2. Write `~/.claude/skills/council/SKILL.md` that: captures the session's
   plan/decision into an artifact file, runs `council new`, then loops
   `critic → (you revise + respond) → revalidate` until `finish`, surfacing the
   objections/convergence to the session each round.
3. Add artifact auto-detection later (`/council` with no args → council the plan in
   context); decision/diff detection is a refinement.

**Gotchas for a continuing session:**
- This tree is shared with a parallel session (it has touched `llm/content.go`,
  `provider/openaicompat/`, `cmd/gemdump/`). When committing council work, stage only
  `council/`, `cmd/council/`, and the council docs — never `git add -A`.
- The cost table in `council/record/cost.go` is **estimated** (flagged in records). Real
  prices reconcile against [[model-cost-map]]; the resolved model id is stored so cost
  recomputes later.
- `agent.ReadOnlyTools` still includes `run` (it is not truly read-only) — matters for
  slice 4 (code mode), not slices 1–3.

## The question, and where it came from

Observation from the model-behavior evals: **GPT-5.5, pointed at our own work,
reliably finds real improvements** — it critiques the harness, the plans, the
code. The idea: formalize that into a council where Claude (the author) and
GPT-5.5 (the critic) argue toward a better artifact, with a cheap third model
refereeing. We tested it twice for real before designing anything.

### Grounding run 1 — plan mode, single critic call

GPT-5.5 reviewed the council *plan itself* (949 prompt / 1532 completion tokens,
36s). It found 8 objections; the strongest (O1/O3): **"ledger empty" means
*procedurally addressed*, not *correct*** — a cheap referee that judges "is this
rejection plausible" rewards confident rationalization. It also emitted O3 as a
near-duplicate of O1 — live proof of its own O2 ("you have no dedup mechanism").
The Gemini referee then **rubber-stamped all 8 as resolved** because the author
merely *said* it would fix them. That rubber-stamp is the single most important
empirical finding and it shapes the whole design below.

### Grounding run 2 — code mode, read-only exploring critic

GPT-5.5 ran as a read-only agent (`agent.RunNative` + `ReadOnlyTools`) over the
real repo, then critiqued *this very design* citing real functions. First attempt
**hit the iteration cap still reading** (22 turns, never answered) — a live
confirmation that a code-mode critic needs an explicit read budget. With a budget
it answered in 8 turns, heavy cache reuse (208k/279k cached). Its findings are
folded in below; the run records would have been the first corpus entries.

## Honest framing — what the council is, and what it isn't

It is **not** a symmetric two-model debate. Run 1 showed why: symmetric argument
collapses into either politeness or stubbornness, and a cheap referee judging
author intent is worse than useless. The real value is an **asymmetric critic**.
So the shape is:

- **Author = the live Claude Code session.** It holds the artifact, revises it, or
  rejects an objection with a cited reason. It is "free" — it's the session that's
  already running. **Critically, the author is NOT a driver-os agent:** there is no
  `provider/anthropic` yet (the `provider/` tree is `openaicompat` only, and
  `cmd/agent`'s `pickProvider` selects OpenRouter or XAI). driver-os literally
  cannot call Claude today, which *forces* the author to be the session and the
  orchestration to live in the skill.
- **Critic = GPT-5.5** (flagship coding model per the model-selection policy),
  invoked through the harness.
- **Referee = Gemini cheap** (`gemini-3-flash`) — and it is **exception-only**, not
  every-round. Its only defensible job is mechanical: it does **not** certify
  semantic closure. Closing a high/medium objection requires the **critic** to
  re-read the revised artifact (round N+1), or executable evidence. The referee
  fires *only* to break an author-says-resolved / critic-says-open disagreement.

So the "three-way handshake" is really a **2-way hot path (Author ↔ Critic) plus an
exception-only referee**, with a deterministic ledger as bookkeeping.

## Architecture — a feature on top of the brick

```
driver-os (the brick — unchanged)
  agent.RunNative + a strict read-only toolset   → code-mode critic
  llm.Generate via openaicompat                  → plan/decision critic + referee
  RunResult.Steps[] + llm.Usage                  → the per-call telemetry we record

NEW feature (this note):
  council/            orchestration: ledger state machine, role drivers
  council/record/     the run-recording library — the system of record
  cmd/council/        the CLI the skill calls; owns the run dir + persistence

~/.claude/skills/council/SKILL.md   installed host-wide → /council in any session
```

**Why the skill drives the loop, not the CLI.** The author is the session, so the
loop's middle step ("revise or reject") happens *in the session's head*. The skill
therefore orchestrates rounds and calls `cmd/council` per action; the CLI runs the
non-Claude roles and records everything. Per-action subcommands, each appending to
one run directory:

- `council new --mode plan|code|decision --artifact <f>` → creates run dir, prints `run_id`
- `council critic --run <id>` → GPT-5.5 pass; records the full call; prints objections (JSON)
- `council respond --run <id> --from <f>` → records the author's revise/reject per objection
- `council referee --run <id> --objection <id>` → exception-only tiebreaker; records
- `council status --run <id>` → convergence state (open high/med, rounds, signal ratio)
- `council finish --run <id>` → finalize manifest + append to the global index
- `council action --run <id> --action shipped|revised-and-shipped|abandoned|superseded|other [--commit <sha>] [--note <text>]` → records, post-hoc, what the human DID with the verdict; stamps the manifest, appends a `human_action` event, and mirrors the disposition onto the run's index line. This is the loop-closer: it turns "we recorded a debate" into "did the debate help" — the corpus can now correlate a converged council with whether the reviewed work actually shipped.

Nothing happens off the record: every subcommand writes. Every record carries a
`schema_version` (`record.RecordSchemaVersion`) so a corpus reader can refuse an
incompatible shape rather than misread an absent-vs-zero field. The run rollup
(`Totals.Referee`) tallies referee adjudications (disputes / upheld-author /
upheld-critic) so you can see whether the referee is a real tiebreaker or a rubber stamp.

## Modes — the critic call is not one shape (grounding run 2, objection #3)

A tool-using critic round is a *full agent run* of many serial model calls (we saw
8–22 iterations), not "one GPT call." So the mode determines the critic's shape:

- **`plan` / `decision`** — critic = a single `llm.Generate` over the captured
  artifact. No tools, no agent loop, no detectors. One call (~36s). This is the
  cheapest, simplest path and the one both grounding runs exercised first.
- **`code`** — critic = `agent.RunNative` with a **strict** read-only toolset and an
  explicit read budget. See the open issues on `ReadOnlyTools` and detectors below.

## Ledger state machine — the author may not self-close

The ledger is deterministic bookkeeping (objection identity, history, status); the
*judgment* lives with the models. Objection states and who may set them:

- `open` — set by the **critic** when it emits the objection.
- `claims-resolved` — set by the **author** after a revision. **Not** terminal.
- `rejects-with-reason` — set by the **author**; must cite something concrete.
- `resolved` — terminal; for high/medium, set **only** by the **critic** on re-read
  (or by executable evidence later). The author can never set this.
- `rejected-justified` / `still-open` — set by the **referee**, only when adjudicating
  a disagreement.

Convergence = no `open` and no unresolved high/medium objections, OR round == cap
(default 3; a higher explicit cap is allowed, never unbounded — there is always a
hard token/$ ceiling that aborts the run).

## The record model — every detail, verbatim, local

Decisions: store under the **XDG data dir**; capture **everything verbatim**;
**local-only** (the records are never sent anywhere).

```
$XDG_DATA_HOME/driver-os/council/        (default ~/.local/share/driver-os/council/)
  index.jsonl                            one line per finished run (the queryable corpus)
  runs/<ts>-<shortid>/
    manifest.json                        run metadata + final rollup
    events.jsonl                         append-only, one typed event per line (the history)
    ledger.json                          current ledger snapshot (history lives in events)
    artifacts/v0.md, v1.md, …            the artifact at every round
```

Append-only `events.jsonl` so a multi-invocation run is never lost mid-flight. Each
event is `{ts, type, ...}`. The event types:

**`run_start`** — mode; **resolved** model id per role captured *with* its date
suffix (`openai/gpt-5.5-20260423`) for reproducibility; config (round cap, token/$
budget, detector settings, read budget); `repo{path, head_sha, dirty}`; host; Claude
session id if exposable; `driver_os_commit`; skill version.

**`model_call`** — the heart of the corpus. Per call:
```json
{ "ts": "...", "type": "model_call", "round": 1, "role": "critic",
  "model": "openai/gpt-5.5-20260423",
  "request":  { "system": "...", "messages": [...], "tools": [...],
                "schema": {...}, "max_tokens": 4000, "sampling": {...} },
  "response": { "text": "...", "finish_reason": "stop" },
  "usage": { "prompt": 949, "completion": 1532, "total": 2481, "cached": 0 },
  "latency_ms": 35831, "cost_usd": 0.0163,
  "agent": { "outcome": "answered", "iterations": 8, "steps": [ /* full RunResult.Steps */ ] } }
```
`request`/`response` are stored **in full** (the system prompt, every message, the
raw reply). For code-mode the `agent` block embeds the harness's entire
`RunResult.Steps[]` — every tool call, observation, and per-turn usage — so the full
exploration trace (the one we watched hit_cap then answer) is replayable. `cost_usd`
is `usage × price` from the model-cost map.

**`objection`** — `{round, id, canonical_key, claim, severity, evidence, fix, dedup{is_dup, dup_of}}`.
**`author_response`** — `{round, objection_id, action: "claims-resolved"|"rejects-with-reason", text, artifact_version_after}`.
**`ledger_transition`** — `{objection_id, from, to, by: "critic"|"author"|"referee", reason}` — the audit trail proving the *critic*, not the author, closed each high/medium objection.
**`referee_verdict`** — only on disagreement.
**`round_end`** — `{round, open_high_med, new_objections, signal_ratio_so_far}`.
**`run_end`** — `{outcome: "converged"|"hit_cap"|"aborted"|"refused", rounds, totals{tokens_by_role, tokens_by_model, cost_usd, wall_clock_ms}, signal_ratio, final_artifact_version, error?}`.

`signal_ratio` = objections that caused a real revision ÷ total — the headline
quality metric, and the nitpick-inflation guard.

**The global index** — `index.jsonl`, one line per finished run
`{run_id, ts, repo, mode, outcome, rounds, cost_usd, tokens, signal_ratio, models}`
— makes the whole host's council history queryable in one `jq` and loadable
straight into the eval harness (HP-11). *This* is the dogfood payoff: which models
raise which objection types, cost-per-resolved-objection, convergence rounds,
cache-hit rates, where critics hit_cap.

## Storage vs egress — two separate gates (grounding run 2, objection #7)

The records store full prompts, which contain your code/diffs, in plaintext. That
is fine for a **local** store on your own host and we keep it **unredacted** (store
everything). Egress is a *different* gate: a council sends that context to
OpenRouter, so before any external send the skill must surface **what will leave the
session** and get confirmation, and honor a provider/model allowlist. Storage =
local and total; egress = confirmed and bounded.

## What we borrow from the brick, and the gaps to close in the feature

- **Telemetry for free.** `RunResult{Steps, Usage, Outcome, Iterations}` and
  `llm.Response{Model, FinishReason, Usage}` already expose almost every field the
  record needs. The recorder mostly serializes what the harness returns.
- **`ReadOnlyTools` is not read-only** (grounding run 2, objection #8): it keeps
  `run`, which can touch the filesystem. The code-mode critic must use a *stricter*
  toolset (drop `run`, or run in a disposable docker/worktree sandbox). The council
  builds its own critic toolset; it does not weaken the harness's.
- **No structured-output field** (objection #9): `llm.Request` has no JSON-schema
  field for the *final* answer (tool *args* are schema'd, answers aren't). The
  council adds a small structured-generation helper in `council/` (request JSON →
  validate against the objection schema → retry once → canonicalize/dedup). If the
  harness later grows a response-format field, the helper collapses onto it.
- **Coding-tuned detectors** (objection #4): `RunNative`'s repeat / `noProgress` /
  `maxStagnant` detectors can falsely kill a critic that legitimately re-reads files
  or re-runs a check. Code-mode passes relaxed/disabled navigation detectors + a read
  budget instead of edit-loop heuristics.
- **Per-objection executable evidence** (objection #5) is a *later* capability:
  `VerifyCmd` is a whole-run closing gate, not per-objection. A future ledger
  `evidence_cmd` runs in the clean `VerifySandbox`. Not in v1.

## Build order

1. **`council/record` + `cmd/council` skeleton, plan mode only.** *(SHIPPED)*
   `new` / `critic` (single `llm.Generate`) / `respond` / `status` / `finish`, with
   the full record model and the global index, plus the structured-objection helper.
2. **Round-N critic re-validation + the exception-only referee.** *(SHIPPED)*
   `revalidate` has the critic re-read the revised artifact and verdict each pending
   objection (closed/stands); agreement closes terminally (by critic), a "stands"
   that contradicts the author's claim becomes a Dispute the referee breaks. New
   objections raised in the pass enter `open`. Convergence = all high/med terminal.
3. **The host-wide skill.** *(SHIPPED)* `~/.claude/skills/council/SKILL.md` drives the
   loop in any session; `make install-council` puts the binary on PATH; egress
   confirmation before the first critic call; artifact picked from `$ARGUMENTS` file or
   the plan in context. Records still stamp `SkillVersion`; bumped to `slice3`.
4. **Code mode.** *(SHIPPED — round-1 grounded review; multi-round deferred.)*
   `council new --mode code --repo <path> [--read-budget N]` runs the critic as a
   read-only `agent.RunNative` over the repo (`council/critic_code.go`,
   `RunCriticCode`). Strict toolset `CodeCriticTools` = exactly `{list_dir, read_file,
   search}` (allowlist-asserted; `run`/write/edit dropped) + a secret deny-list
   (`sensitivePath` over read_file & search). Read budget = `MaxIterations`
   (`--read-budget`, default 25). New opt-in harness knobs: `NavSpiralWindow` (relaxes
   the explore-spiral detector for a top-down survey) and `AnswerNudgeWindow` (a
   near-cap answer-forcer that fires ONLY for an observe-only toolset — fail-closed).
   The full agent trace is recorded in `model_call.agent` (`AgentTrace`).
   **Known gaps for the next increment:** (a) code-mode `revalidate` is gated off —
   prose-only re-read would falsely close code-grounded objections (O2), so a
   repo-grounded re-read is still owed; without it code mode is a single review round.
   (b) ~~code-mode `finish` then labels the open objections `hit_cap`/signal-0; it needs a
   "reviewed" outcome.~~ **CLOSED (2026-06-03):** `council.FinalOutcome` (the one outcome
   policy shared by `finish` and the new `council reindex`) maps a code review whose
   critic delivered an answer to `reviewed` — its open objections are findings, not
   blockers. The same function also stops `finish` from ever writing a transient
   `in-progress`/`awaiting-revalidation` outcome to the corpus (an early finish with
   work still open is `aborted`). `council reindex [--dry-run]` rebuilds `index.jsonl`
   from the per-run manifests, dropping never-finished runs and self-healing rows an
   older `finish` mislabeled (it corrected the 3 bad rows in the live corpus: the two
   transient plan runs → `aborted`, the code run → `reviewed`). (Container/`ProcessHost` help per docs/specs/SESSION.md but the council does
   not depend on them — the local read-only sandbox is enough for a no-`run` critic.)
5. **Eval ingestion.** Load `index.jsonl` into the eval harness (HP-11): does
   councilled output beat Claude-alone, per extra $/token, on a fixed benchmark?

## Slice 1 dogfood findings (2026-06-03)

The first real plan-mode run was the council reviewing its own slice-1 design. It
immediately found — and we fixed — three things, the corpus surfacing each:

- **Dedup could drop a high-severity objection** (council O3). A duplicate now
  UPGRADES its canonical group to the max severity, so a high dup of an earlier low
  is still counted toward convergence. Regression-tested.
- **Unknown-priced calls summed to $0 silently** (council O5). `Totals` now carries
  `cost_incomplete` so a missing price can't masquerade as a complete, cheap run.
- **A reasoning critic truncates at a low token cap** (found in the records, not by
  the critic). The first call hit `finish=length`, spent all 2200 completion tokens
  on reasoning, emitted *empty* content, and forced a retry — ~2× cost. Fix: default
  cap raised to 4000, and the retry now RAISES the cap on a length-truncation instead
  of re-sending the same one. Confirmed: the same artifact now completes in one call.

## Slice 2 convergence evidence (2026-06-03)

A full plan-mode council run drove a deliberately-weak artifact to **converged** in
3 rounds (live GPT-5.5 critic + Gemini referee, $0.048 total). The run is the proof
the rubber-stamp is dead:

- Round 1: critic raised 8 objections. Author revised + answered (7 fixes, 1 reasoned
  rejection).
- Round 2: critic re-read v1 — closed 6 (incl. accepting the rejection as
  `rejected-justified`), said 2 stand → both went to the referee, which **upheld the
  critic** (`still-open`) because the fixes were weak. It also raised 2 new objections.
- Round 3: author revised again (v2). Critic re-read — the same 2 went to the referee,
  which this time **upheld the author** (`resolved`) because v2 genuinely fixed them.
  All high/medium terminal → converged.

The referee adjudicated in **both** directions (uphold-critic when the fix was weak,
uphold-author when it was solid) — real adjudication, not the slice-1 rubber-stamp. It
fired only on the 4 disputes, at ~$0.00016 each (~100× cheaper than a critic call),
confirming the exception-only/cheap-referee design. Every status transition in the
record carries a `by` of author/critic/referee — a complete audit trail.

## Open questions / risks (not yet decided)

- **Author honesty.** The author (the session) grading its own revisions is the
  conflict the critic-closes-it rule exists to break — but the author still *writes*
  the `rejects-with-reason`. Is critic re-read enough, or does a stubborn author need
  a harder gate? Tie to HP-5.
- **Signal-ratio as a stop signal.** If round 1 yields only low-severity objections,
  stop. The threshold is a guess until the corpus says otherwise.
- **Cross-session concurrency.** Two sessions running councils write to the same
  store; `index.jsonl` appends must be atomic (O_APPEND line writes, or per-run files
  the index is rebuilt from).
- **Session id capture.** Whether a skill can read its Claude Code session id to tag
  records is unconfirmed; degrade gracefully if not.
