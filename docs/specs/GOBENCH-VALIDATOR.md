# GOBENCH-VALIDATOR — build spec for `cmd/gobench-validate` (Phase 3)

Status: DRAFT (2026-07-07). Owner: Robin. Plan of record: `docs/specs/GOBENCH.md`
Phase 3. This is the moat: candidates in, validated instances or rejections out.

Scope of Phase 3 = the eight validator stages (GOBENCH.md §Phase 3) plus the three
G1 follow-ups (`docs/findings/GOBENCH-G1-REPRODUCTION.md`). We build it in slices so
no single driver-os task is too large and the budget can stop cleanly at any slice
boundary.

## What already exists (reuse, do not rebuild)

Package `eval/suite/gobench/` (mapped 2026-07-07):

- `Instance` / `Validation` / `RunResult` / `LeakScreen` / `Verdict` / `OracleTest` /
  `TestID` / `PassToPass` / `ExecSpec` types (`instance.go`). The validator POPULATES
  `Validation.{RedAtBaseRuns,GoldGreenRuns,FlakeRuns,ValidatorVer,CreatedAt,LeakScreen}`
  and stamps `ValidatedAt`.
- `CheckoutBase(ctx, repoURL, commit, destDir, cacheDir)` (`checkout.go`) — works for
  ANY commit, so it materializes both base and gold. `EnsureBareMirror` shares one
  mirror across both checkouts.
- `Grade(checkoutDir, oracleDir, inst) (Verdict, error)` (`grader.go`) — already
  overlays oracle files, resets `*_test.go`, runs F2P `-v` with test-name-ran
  verification, runs P2P. Returns `Verdict` incl. `RanTests`. **The gold-green stage
  IS essentially a `Grade` call at the gold checkout that must return Resolved=true.**
- Low-level primitives in `grader.go` (in-package): `runGoTest`, `parseGoTestVerbose`
  (+ `goTestStatus{ran,passed,failed}`), the argv builders, `referencedPackages`,
  `combinedRunRegex`. The K-times flake loop and the red-at-base check build on these.
- `AssembleCandidate` / `FilterPR` / miner (`mine.go`, `cmd/gobench-mine`) produces
  the partial `Instance` the validator consumes (all `Validation` fields zero).

**Design decision — the validator lives IN package `gobench`** (a `validate.go` file
+ a thin `cmd/gobench-validate/main.go`), matching grader/miner. This gives it direct
access to the unexported `runGoTest`/`parseGoTestVerbose` primitives with zero export
churn. `cmd/gobench-validate/main.go` holds only flag parsing, I/O (issue-body fetch
for the scrub stage), and orchestration.

## Slice 1 — schema + toolchain plumbing (SHARED GROUNDWORK) — DELEGATED 2026-07-07

Lands the two code capabilities everything downstream needs. Independent of the
validator command itself.

1. `PassToPass.RunRegex string \`json:"run_regex,omitempty"\`` + grader threads it as
   `-run <regex>` into every P2P `go test`. (G1 follow-up #2.)
2. `ToolchainEnv(goVersion string) []string` → `["GOTOOLCHAIN=go<ver>"]` or nil;
   `Grade` applies it to testbuild + F2P + P2P when `inst.GoVersion != ""`. (G1
   follow-up #1.)

Gate: `go build ./... && go vet ./eval/suite/gobench/... && go test
./eval/suite/gobench/...` green; existing round-trip golden fixtures byte-stable.

## Slice 2 — validator core (THE MOAT) — DONE 2026-07-07 (`bd0783a`)

Shipped `eval/suite/gobench/validate.go` + `validate_test.go` (gpt-5.5 solver +
opus-4.8 review, $0.76, review clean). `overlayOracle` extracted from `Grade` (DRY,
Grade behavior unchanged). Pure classifiers `classifyRedAtBase`/`classifyGoldGreen`
carry the offline unit tests; flake-quarantine is folded into the red/gold K-loops.

**KNOWN GAP (G3 follow-up): the timebox stage is inert.** `runGoTest` hard-kills at
`testTimeout`, and `firstSlow` flags `DurationS > testTimeout`, so a genuinely slow
run is killed (surfacing as `gold-red`/`flaky`) before it can ever be flagged `slow`.
The stage needs a SEPARATE slow-threshold below the kill timeout. Fold this into the
Gate G3 slow-tier-vs-reject decision.

`validate.go`: `func Validate(ctx, inst Instance, opts ValidateOpts) (Instance,
*Rejection, error)` — returns the fully-populated instance on accept, or a `*Rejection`
with a taxonomy `Reason`. Pure of flag/CLI concerns. Stages, in order, first failure
short-circuits to a `Rejection`:

1. **Environment / broken-base.** `CheckoutBase` at `inst.BaseCommit`. Pin toolchain
   (`ToolchainEnv(inst.GoVersion)`). `go build ./...` at base must be green — else
   `Rejection{Reason:"broken-base"}`.
2. **Isolation / overlay-collision.** Overlay ONLY `inst.OracleFiles` (from the
   oracle dir extracted at mining time — NEVER re-derive from a live merge object;
   gotcha #8b) onto the base checkout, then compile the test binary of every
   referenced package (`go test -c -o /dev/null <pkg>`, toolchain-pinned). A compile
   failure here = `Rejection{Reason:"overlay-collision"}` (a duplicate-symbol / API
   drift, NOT a red). This runs BEFORE red-at-base so a collision is never
   mis-scored as a fail.
3. **red-at-base.** With the oracle overlaid on base, run F2P `-v` (reuse
   `parseGoTestVerbose`); require every named test to RUN and FAIL. If any named test
   did not run → `Rejection{Reason:"f2p-did-not-run"}` (the testify `TestSuite/Method`
   `-run`-miss trap; gotcha #8a). If all named tests RAN and PASSED (green at base) →
   `Rejection{Reason:"no-gate"}` (the cobra/go-git failure mode; expect ~half of
   candidates here).
4. **gold-green.** `CheckoutBase` at `inst.GoldCommit`; `Grade` there must return
   `Resolved=true` (F2P passes AND P2P — the touched packages / P2P run-filter subset
   — stays green). Else `Rejection{Reason:"gold-red"}` (upstream flake or co-changed
   code the instance can't isolate).
5. **Flake quarantine.** Repeat stage 3's red-check K× at base and stage 4's green
   check K× at gold (`opts.FlakeRuns`, default K=5). ANY flip (a base run passes, or a
   gold run fails) → `Rejection{Reason:"flaky"}`. Record EVERY run as a `RunResult`
   (`Passed`, `RanTests`, `DurationS`) into `Validation.RedAtBaseRuns` /
   `GoldGreenRuns`; set `Validation.FlakeRuns = K`. (Stages 3/4 are effectively the
   first iteration of this loop — implement as one K-times loop, not four separate
   passes, to avoid redundant checkouts: checkout base once, run K×; checkout gold
   once, run K×.)
6. **Time-box.** If any package's test wall-time exceeds `inst.TestTimeout`
   (default 10m) → `Rejection{Reason:"slow"}`. (Whether "slow" is a hard reject or a
   separate slow-tier is decided at Gate G3 — for now, reject and record the
   duration so G3 can see the distribution.)
7. **Determinism receipt.** On accept, stamp `Validation.ValidatorVer` (a
   `const ValidatorVersion = "gobench-validate/0.1.0"`), `Validation.CreatedAt` (PR
   merge date — carry from the candidate or the miner), `ValidatedAt` (an injected
   clock — DO NOT call `time.Now()` inside `Validate`; pass a timestamp string in
   `ValidateOpts` so tests are deterministic), and re-run
   `Instance.Validate()` (structural) before returning the accepted instance.

Stage 7 (problem-statement scrub) and the leak-screen are Slice 3 — a validated
instance from Slice 2 carries the RAW `problem_statement` and an empty `LeakScreen`;
Slice 3 fills them.

`ValidateOpts`: `{OracleDir string, CacheDir string, FlakeRuns int, Now string,
BuildTimeout, TestTimeout time.Duration}`.

Testing (offline): the stages that shell out to `go`/`git` are exercised by a live
run, not the unit gate. Unit-test the pure decision logic: a fake run-result →
verdict mapping (does a base-pass flip to `flaky`? does a gold-fail → `gold-red`?),
the K-times aggregation, and `RunResult` accumulation, by factoring the pure
stage-decision functions apart from the `runGoTest` I/O (the same seam pattern the
miner uses — pure `mine.go` vs I/O `main.go`).

Gate G3 (partial, this slice): re-run `Validate` on the 5 hand-built instances'
raw base+gold and confirm the accept/reject verdict matches the manual work
(lipgloss/urfave/opa/dolt accept; prometheus is the flake-quarantine target — it
should now either pass with the toolchain pin + P2P `-run` filter, or reject `flaky`,
and either outcome is CORRECT and documented).

## Slice 3 — problem-statement scrub + leak-screen (stage 7 + the contamination axis)

Separable, involves an LLM call and a text-similarity screen.

- **Scrub.** A cheap-model pass (`deepseek/deepseek-v4-flash` — the prose/triage
  standout) that strips fix hints, PR links, and stack-trace lines naming the fix
  location from the RAW issue body, keeping VERBATIM symptom text + expected
  invariant (issue-text call resolved 2026-07-06: verbatim-scrubbed, not link-only).
  Human spot-check of every release-1 instance — so the tool WRITES a diff (raw →
  scrubbed) for review, it does not silently overwrite. Output → `problem_statement`;
  raw stays available for audit.
- **Leak-screen** (G0 NEW RISK, binding): an n-gram overlap screen between the
  scrubbed `problem_statement` and the gold diff — if the statement contains the fix,
  the contamination bound is worthless. Populate the `LeakScreen` struct
  (`method`, `ngram_size`, `score`, `threshold`, `passed`, hashes, `screened_at`). A
  score over threshold flags the instance for human review, does not auto-accept.

## Slice 4 — `cmd/gobench-validate` wiring — DONE 2026-07-07 (`4ea4c2f`)

Shipped `cmd/gobench-validate/main.go` (+ `main_test.go`). gemini-3-flash solver +
gpt-5.5 review ($0.54); review caught 3 findings, solver repaired 2, the 3rd
(exit 0 on missing/unreadable candidates dir) fixed directly (`os.Stat` guard) and
verified against the reviewer's own repro. Per-candidate `OracleDir =
<oracles-root>/<instance_id>`; infra faults become a synthetic `{stage:infra,
reason:error}` rejection so no candidate is dropped silently.

Original design notes below.

Thin `main.go`: flags (`-candidates`, `-oracles`, `-out`, `-cache`, `-flake-runs`,
`-repos`, `-ids`), fetch issue bodies for the scrub stage (`gh issue view`, the same
I/O seam the miner uses), drive `Validate` over a candidate dir, write accepted
instances to `<out>/instances/` and a validation rejection log
`<out>/validation-rejections.jsonl` (same `{repo,pr,stage,reason}` shape as the
miner's). Mirror `cmd/gobench-mine/main.go` structure.

Gate G3 (full): validator re-derives the 5 hand-built instances matching manual
work; on a fresh repo batch, human audit of 10 random accepts + 10 random rejects
finds ≥9/10 correct each direction; yield vs the R0.3 estimate reported; the
slow-tier vs reject decision (stage 6) made.

## Live smoke-test 2026-07-07 — pipeline PROVEN, two gaps found

First end-to-end live run of the `mine → validate` pipeline (no driver-os; ran the
binaries directly). Mined urfave/cli (20 crawled → 7 survivors) and opa (15 → 3),
then validated `urfave__cli-2363` — a canonical hand-built instance — with K=2.

**Result: ACCEPTED end-to-end** — toolchain pin (go1.22.0) → `go build ./...` base →
isolation → red-at-base ×2 (F2P ran and FAILED at base) → gold-green ×2 (Resolved) →
receipt (`RedAtBaseRuns`/`GoldGreenRuns` populated, `ValidatorVer`/`ValidatedAt`
stamped). Matches the manual verdict — a live preview of Gate G3 part (a).

Two gaps the smoke-test surfaced:

1. **FIXED (`2a657bb`) — bare major.minor `go_version` broke GOTOOLCHAIN.** urfave's
   go.mod declares `go 1.22` (a language version), so `ToolchainEnv("1.22")` emitted
   `GOTOOLCHAIN=go1.22`, which the go tool rejects. `ToolchainEnv` now normalizes a
   bare major.minor to `major.minor.0`. Unit tests missed it (used 3-part `1.25.7`);
   only a real 2-part go.mod exposed it — the value of driving the real flow.

2. **OPEN (G3 follow-up) — the `environment` stage conflates infra failure with a
   genuine broken-base.** The first opa run rejected `broken-base` with detail
   `disk quota exceeded` (opa's `go build ./...` overflowed a 16G tmpfs `/tmp`). A
   disk-quota / OOM / network / invalid-toolchain failure is NOT a property of the
   instance — it should be an infra `error` (retry/abort), not a `broken-base`
   rejection (same spirit as the grader's testbuild-vs-test-failure split). The
   `environment` stage should pattern-match known infra-failure signatures
   (`disk quota exceeded`, `no space left`, `cannot find module`, `invalid toolchain`,
   network timeouts) and classify them `error`, reserving `broken-base` for a build
   that fails on the code itself. **Operational note:** run validation with build
   scratch on a large FS — `TMPDIR`, `-cache`, `-out` on `/home` — not the tmpfs
   `/tmp`; opa's build alone exceeds a 16G tmpfs.

## Sequencing decision 2026-07-07 — MOAT FIRST (binding until revisited)

External critical review (2026-07-07) found the program's differentiators are
exactly the unbuilt parts, and that shipping instance plumbing ahead of them
converges on "another small SWE-bench for Go". Binding order before any BATCH
validation run or wave-1 instance publication:

1. **Red-at-base strict per-test invariant** — fix landed 2026-07-07 (see
   below): a run is red only if EVERY named F2P test ran AND failed; partial
   pass at base → `no-gate` naming the non-gating tests. (The loose per-run
   boolean silently accepted instances an empty patch passes — found by
   post-hoc flagship review after both delegation gates missed it; the bug
   lived in the untested I/O seam.)
2. **Slice 3 (scrub + leak-screen)** — the contamination axis is G0-binding;
   no wave-1 instance ships without it.
3. **Honesty axis `claim` field + SYMMETRIC scoring** — the current definition
   scores driver-os structurally but other harnesses via an LLM reading their
   final message: a harness that always hedges is never false-green. Before
   cross-harness honesty numbers are published, the claim signal must be
   defined symmetrically (or the asymmetry declared in the spec) and the
   classifier's agreement with the structural signal measured on driver-os
   runs where BOTH exist. Free calibration corpus: the 2026-07-07 windowed-
   reads A/B logs contain ~40 oracle-confirmed false-positive gemini trials
   (eval/runs/read-outline-ab-20260707T131955Z).
4. Only then: batch validation / G3 part (b) at scale.

## Council-hardened launch decisions that bind the validator (2026-07-07)

The public-launch plan (`docs/specs/GOBENCH-LAUNCH.md`, council run
20260707-195325-c18f08) added validator-relevant requirements beyond the
slices above. None block Slice 3 as specced; they are the next increments:

- **`statement_mode` (schema addition, post-Slice-3):** exactly two values —
  `verbatim-scrubbed` (only where redistribution rights are demonstrably
  clear) and `authored-summary` (DEFAULT under rights uncertainty: a
  GoBench-authored statement, CC-BY-4.0, no third-party text). Both modes
  pass the same leak-screen. **No live-fetch mode exists**: the canonical
  prompt is frozen at mine time; the miner records `source_fetched_at` + a
  content hash of the source issue; validation FAILS any instance whose
  prompt reconstruction depends on live mutable content.
- **Provenance fields (validator-enforced, launch-blocking):**
  `license_spdx`, `statement_source_url`, `statement_mode`, author
  attribution — missing = reject. CC-BY-4.0 is scoped to GoBench-authored
  content only; repo code/tests/diffs stay reference-by-SHA.
- **Outcome taxonomy is grader-decided (resolves item 3 above):** the launch
  plan pins `answered` as artifact-derived — final patch git-applies cleanly
  + no machine-readable abstain marker; emitting a patch and stopping IS the
  claim (closes the always-emit loophole; prose hedging alongside a patch
  does not demote). This replaces the "LLM reads the final message" plan for
  cross-harness claim scoring; the LLM classifier survives only as a
  diagnostic to be calibrated against the structural signal. Five published
  columns: attempt / resolve / false-green / invalid-patch / abstention.
- **Infra-vs-broken-base** (already OPEN below) is promoted to
  launch-blocking by the plan's honesty gates.

## Gotchas the validator MUST honor (from HARNESS-VS-BIG3 gotcha #8)

- **#8a** — subtle multi-file bugs often DON'T gate on a test-only overlay (the gold
  test passes at base because the bug needs co-changed code). This is stage 3's whole
  job: red-at-base with `-v` and verify names actually RAN. Expect ~half of
  candidates rejected `no-gate`.
- **#8b** — never rely on a live merge object at grade time. Oracle test files are
  extracted from gold at MINING time and stored; the validator overlays the STORED
  files. A shallow/GC'd cache must never silently produce a false green-at-base.
