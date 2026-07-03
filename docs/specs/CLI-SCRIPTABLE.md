# CLI-SCRIPTABLE — Tier 1 spec

Goal: make a **single `cmd/agent` run** obey the Unix contract, so it can be
driven from a pipe, a Makefile, or CI without a human. Scope is one run; the
run *lifecycle* (list/show/replay), binary unification, and config profiles are
out (Tier 2–4, see below).

A script's loop is: **feed input → branch on exit code → parse stdout.** Today
all three are broken — stdout mixes prose with the answer, every non-answer
collapses to exit 1, and `-review` blocks on a TTY. Tier 1 fixes exactly those.

The leverage is that the two hard pieces already exist: the `Observer` seam
(`agent/observer.go`) is already an event stream, and the `Outcome` taxonomy
(`agent/agent.go:154`) is already 11 typed terminal states. Tier 1 is plumbing
them to the process boundary, not redesign.

## Decisions

### D1 — Two streams: stdout is the data channel, stderr is diagnostics
**stdout** carries the run's *data*; **stderr** carries only unstructured
diagnostics — the live human trace and the startup banners (`sandbox:`,
`protocol:`, `memory:`, `transcript:`). What "the data" means depends on format:

- `text` / `json` — stdout is a **single final payload** (the SUMMARY/answer, or
  the result object). The live trace is diagnostics → stderr.
- `ndjson` — the data channel **is** the event stream (D2): every `Observer`
  event *and* the terminal `result` event go to stdout as the run progresses.
  stderr still carries only the unstructured banners, never structured events.

This moves the observer trace off stdout for the default `text` mode — a behavior
change, but today's `text` stdout is not machine-parseable anyway, and
interactively stderr still renders on the terminal, so it is strictly better for
redirection and lossless for a human watching.

### D2 — `-format text|json|ndjson` (default `text`)
- `text` — today's human SUMMARY line + answer, to stdout. Trace to stderr (D1).
- `json` — one result object (schema below) to stdout, at exit. Nothing else.
- `ndjson` — one JSON object per `Observer` event to stdout as the run
  progresses, terminated by a final `result` event. A consumer that only wants
  the result reads the last line (or filters `type=="result"`).

The `ndjson` event shape maps 1:1 onto the existing `Observer` interface:

```
{"type":"iteration","i":1,"max":8}
{"type":"model","text":"read_file main.go | search RunNative"}
{"type":"observation","text":"exit 0 (12ms)\n..."}
{"type":"note","text":"near cap — nudging to finish (HP-4)"}
{"type":"done","answer":"the module path is ..."}
{"type":"result","result":{ ...result object (D3)... }}   // terminal
```

The terminal event nests the D3 result object under a `result` key (not flattened
into the event), so `type` never collides with a result field. It is the same
object `-format json` emits.

### D3 — The result object (schema_version 3)
Reuses the persisted transcript summary (`agent.RecordFrom`). **Summary, not full
trace** — the complete `Steps` trace is already written to `<id>.json`; we point
at it rather than duplicate it on stdout.

```json
{
  "schema_version": 3,
  "id": "20260603-201500-a1b2c3",
  "outcome": "answered",
  "exit_code": 0,
  "task": "What module path does this project declare?",
  "model": "openai/gpt-4o-mini",
  "answer": "github.com/AccursedGalaxy/driver-os",
  "reason": "",
  "iterations": 3,
  "usage": {"prompt_tokens":1840,"completion_tokens":210,"total_tokens":2050,
            "cached_tokens":0,"reasoning_tokens":0},
  "started_at": "2026-06-03T20:15:00Z",
  "ended_at":   "2026-06-03T20:15:07Z",
  "transcript_path": "/home/aki/.local/state/driver-os/runs/20260603-201500-a1b2c3.json",
  "error": null,
  "review": null,
  "plan": null,
  "solver_cost_usd": 0.0003,
  "reviewer_cost_usd": null,
  "planner_cost_usd": null,
  "total_cost_usd": 0.0003
}
```

**Field presence/nullability is normative** (so strict parsers and tests agree):
*all* top-level keys are **always present**. Specifically:
- `answer` — a string for `outcome` in {`answered`, `unverified`}, else `null`.
  `unverified` *does* expose its answer: an answer exists (it just failed the
  closing gate), and inspecting it is the whole point of the outcome (reconciles
  D4 code 2). The companion `reason` then explains why it didn't verify.
- `reason` — a string for every non-`answered` outcome, else `""`.
- `error` — an object `{kind, message}` for `provider_error`, else `null`.
- `transcript_path` — a string, or `null` if the transcript write failed
  (see below).
- `review` — the review-gate report (rounds, findings, usage), or `null` when
  the gate was off.
- `plan` — the plan-stage report (plan, usage), or `null` when the stage was off.
- `solver_cost_usd`, `reviewer_cost_usd`, `planner_cost_usd` — the USD cost for
  each role's usage when the model is priced in `eval.Pricing`, else `null`.
  Role fields are also `null` when that role was not configured.
- `total_cost_usd` — the sum over the roles that RAN, but `null` if any role
  that ran is unpriced (never conflate unknown with free).

The object is emitted for **every** `RunResult`, including error outcomes (fixes
today's bug where SUMMARY prints only on `err==nil`).

**Transcript-write failure is not a run failure.** The transcript write is
best-effort (it already is in `main.go`). If it fails *after* a valid
`RunResult`, the result object is still emitted with `transcript_path: null` and
a warning on stderr; the **exit code reflects the agent outcome, unchanged** — a
good answer never becomes exit 1 because the disk was full (closes O7).

**Setup errors still emit JSON (exit 1).** D4 exit-1 cases (no provider key,
sandbox build failure) produce no `RunResult`, but once `-format` has resolved to
`json`/`ndjson`, stdout must stay valid JSON so a consumer can parse
unconditionally. Emit a CLI-error object instead of the result object:

```json
{ "schema_version": 3, "outcome": "cli_error", "exit_code": 1,
  "error": { "kind": "no_provider_key",
             "message": "no provider key found; set OPENROUTER_API_KEY or X_AI_API_KEY" },
  "solver_cost_usd": null, "reviewer_cost_usd": null,
  "planner_cost_usd": null, "total_cost_usd": null }
```

The `cli_error` object is wrapped per format so each format stays internally
consistent (closes O9 — the ndjson event contract must not be broken by the
error path):
- `json` — emitted as the **bare object** above (json mode is always one bare
  object on stdout).
- `ndjson` — emitted as the **terminal `result` event**,
  `{"type":"result","result":{ ...cli_error... }}`, because ndjson stdout is
  always typed events. A consumer therefore filters `type=="result"` uniformly
  and branches on `result.outcome` (`cli_error` vs a real outcome) inside.

The one exception is a flag-parse failure *before* `-format` is known — there is
no resolved format to honor, so it exits 1 with the error on stderr only. That is
the consumer's own malformed invocation, distinct from a runtime setup error.

### D4 — Exit codes carry the outcome class
Today: 0 = answered, 1 = everything else. New mapping separates "the harness
never produced a result" (1) from "the agent ran but did not answer" (2–6):

| code | meaning | outcomes |
|------|---------|----------|
| 0 | answered | `answered` |
| 1 | CLI/config/setup error — no `RunResult` produced | no provider key, bad flags, sandbox build failure |
| 2 | unverified — an answer exists (exposed in `answer`, D3) but the closing gate failed | `unverified` |
| 3 | resource exhausted | `hit_cap`, `hit_deadline`, `hit_context_limit` |
| 4 | stuck — a loop detector killed it | `killed_repeat`, `killed_spiral`, `killed_stagnant` |
| 5 | provider/transport error | `provider_error` |
| 6 | refused on policy | `refused_unsafe` |

Rationale for the classes: a script reacts *differently* to each — retry-with-
bigger-budget on 3, backoff-and-retry on 5, never-retry on 6, swap-model on 4,
inspect on 2. Collapsing them would defeat the point.

Under `-format=json|ndjson`, exit 1 still writes a valid JSON `cli_error` object
to stdout (D3) — so a consumer parses unconditionally and branches on the
`exit_code`/`outcome` field, never on whether stdout happened to be empty.

### D5 — Non-interactive gate policies for `-review`
`-review` has **two** blocking prompts; both need a programmatic policy to be
scriptable:
1. per-`run` approval (`stdinApprover`) → `-approve interactive|policy|never`
   (default `interactive`). `policy` runs the existing `gated.DefaultPolicy`
   with no human; `never` denies all gated commands.
2. the final commit/discard/keep prompt → `-review-action prompt|commit|discard|keep`
   (default `prompt`).

So `agent -review -approve=policy -review-action=keep` gates `run` by policy and
leaves the diff unstaged for later inspection — fully non-interactive.

**Interactive prompts read from `/dev/tty`, not `stdin`.** This is what lets
`-task -` (stdin = the task, D6) coexist with `-approve=interactive`: the approver
and the final review prompt draw from the controlling terminal directly, the way
`git`, `ssh`, and `sudo` do — so the task on stdin is never half-consumed by a
prompt, and a prompt is never starved at EOF (closes O3).

**Fail-fast is keyed on the TTY, not the format.** Whenever an interactive policy
(`-approve=interactive` or `-review-action=prompt`) is selected for an *active*
gate and `/dev/tty` cannot be opened (no controlling terminal — CI, a cron job, a
bare pipe), error at startup naming the fix, regardless of `-format`. This covers
the default `text` mode too, where `agent -review` in CI would otherwise still
deadlock (closes O4).

### D6 — Input that composes
- `-model <slug>` and `-provider openrouter|xai` flags. `-provider` omitted ⇒
  infer from whichever key is present (today's behavior). `-model` overrides the
  env default; the flag wins over `*_MODEL`. `-provider` naming a target whose
  key is absent is a startup error (exit 1).
- `-task -` reads the task from stdin (`cat issue.md | agent -task -`). Because
  interactive prompts read from `/dev/tty` (D5), this composes with `-review`:
  stdin feeds the task, the terminal answers the prompts.
- Under `-format=json|ndjson`, an **omitted** `-task` is an error, not the demo
  default — a script must not silently run the canned task. `text` mode keeps the
  demo default for backward compat.

## Non-goals (explicitly Tier 2–4, not this spec)
- Subcommands and run-lifecycle browsing (`agent ls|show|replay`).
- Memory CLI (`agent memory ls|rm|stats`).
- Unifying the 7 `cmd/` binaries under one `driver` entrypoint.
- Config files / profiles; `--dry-run`.
- New providers (OpenAI/Claude adapters). Tier 1 only makes *selection* a flag;
  the adapter gap is orthogonal.

## Open questions (for the grill)
1. **Full trace inline?** D3 emits summary + `transcript_path`. Is a
   `-format json --full` (whole `Steps` array on stdout) needed for consumers
   without filesystem access to the transcript dir, or is the pointer enough?
2. **Default format.** Keep `text` for backward compat, or is there a case for a
   machine default now?
3. **Text-mode trace to stderr (D1).** Strictly-better claim — any real consumer
   that today greps the `text` stdout trace and would break?
4. **Exit-code granularity (D4).** Six classes — right cut, or over/under-split?
   In particular: does `unverified` deserve its own code, or fold into a generic
   "ran but no good answer"?
5. **ndjson schema drift.** Top-level `schema_version` only, or per-event
   versioning?
