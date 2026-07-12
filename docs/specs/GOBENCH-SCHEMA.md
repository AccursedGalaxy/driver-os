# GOBENCH-SCHEMA — the instance contract (§1.1)

Status: DRAFT v1 (2026-07-06). Owner: Robin. Part of Phase 1 of
`docs/specs/GOBENCH.md` (§1.1). Companion: `docs/specs/GOBENCH.md` (§1.2 grader
contract), `docs/findings/GOBENCH-PRIOR-ART.md` (why the schema carries a
leak-screen field), `docs/specs/GOBENCH-RELEASE.md` (why it carries no gold code).

**What this is.** The on-disk shape of one GoBench instance: one JSON object per
instance. Everything downstream — miner (Phase 2), validator (Phase 3), grader
(§1.2), leaderboard (Phase 5) — reads and writes this contract. Get it
council-reviewed before the miner exists (GOBENCH.md Phase 1 gate G1).

**Two design commitments that make GoBench's schema differ from SWE-bench's**,
both load-bearing and both a consequence of Phase 0:

1. **No gold code is ever stored.** SWE-bench's `Instance` embeds `patch` (the
   gold fix) and `test_patch` (the held-out tests) as literal source. GoBench
   stores **`gold_commit` + `oracle_files` (paths only)** and the grader fetches
   the upstream code live at grade time. This is the R0.4 distribution model
   (`GOBENCH-RELEASE.md`): we distribute metadata, never redistribute source or
   test code. The fields that would hold gold code in SWE-bench are **absent by
   construction** here — that is the point, not an omission. Hermeticity is
   preserved by pinning immutable commit SHAs and caching the fetched trees (see
   "Reproducibility & the fetch-live tradeoff" below), not by shipping code.
2. **The problem statement carries a leak-screen receipt.** The R0.1 audit
   (`GOBENCH-PRIOR-ART.md`) found the rolling/contamination story is no longer
   unique (SWE-bench-Live does monthly n-gram leak screening). So a
   `problem_statement` that leaks the fix silently degrades the whole benchmark.
   Every instance records a fully reproducible `leak_screen` receipt; the
   validator (Phase 3) MUST populate it, executed at least to SWE-bench-Live's
   standard.

Port-compatibility (and where we deliberately diverge): GoBench keeps the
upstream JSON keys `instance_id`, `repo`, `base_commit`, `problem_statement`,
`hints_text`, `created_at`, `FAIL_TO_PASS`, `PASS_TO_PASS` so the field *names*
port. But GoBench's F2P/P2P **values are structured objects, not the bare
strings** SWE-bench uses — the extra structure (package qualification, run
regex) is required for unambiguous Go grading (see O1/O2/O3 rationale inline).
The `swebench.Instance` bridge maps by flattening `TestID` → `Name`; that
flattening is lossy and is used only for display/interop, never for grading.

---

## The Go struct

Lives at `eval/suite/gobench/instance.go` (Phase 1.3). Field order below groups
by role; JSON keys are the contract.

```go
// Instance is one GoBench task: a single real Go bug, mined from a merged PR,
// validated red-at-base / gold-green, distributed as metadata only (no gold
// source or test code — the grader fetches upstream at grade time).
type Instance struct {
    SchemaVersion string `json:"schema_version"` // "gobench.instance.v1" — REQUIRED; bump on any breaking change

    // ---- identity ----
    InstanceID string `json:"instance_id"` // "<owner>__<repo>-<pr_number>", e.g. "open-policy-agent__opa-8781"
    Repo       string `json:"repo"`        // "open-policy-agent/opa" — owner/name AT MINING TIME (may rename later)
    RepoID     int64  `json:"repo_id"`     // GitHub numeric repository id — immutable under rename/transfer
    PRNumber   int    `json:"pr_number"`   // the gold PR
    IssueURL   string `json:"issue_url"`   // the linked issue the PR closed

    // ---- commits (the grader checks these out; no code is embedded) ----
    // INVARIANTS (validator enforces): base_commit is the TARGET-BRANCH tip
    // immediately BEFORE the PR was integrated (the red tree the agent starts
    // from); gold_commit is the TARGET-BRANCH tip immediately AFTER integration
    // (the green tree). NEITHER is "the PR parent" in the topological sense —
    // for a merge commit that would be the wrong (pre-merge feature-branch)
    // tree. The validator MUST verify `git diff base_commit..gold_commit`
    // contains the PR's non-test changes; if it does not (stale base, unrelated
    // interleaved merge), the candidate is rejected.
    BaseCommit  string `json:"base_commit"`
    GoldCommit  string `json:"gold_commit"`
    MergeMethod string `json:"merge_method"` // "merge" | "squash" | "rebase" — how the PR landed on the target branch
    GoVersion   string `json:"go_version"`   // from go.mod at base, e.g. "1.22"
    ModulePath  string `json:"module_path"`  // go module import path of the touched module
    ModuleDir   string `json:"module_dir"`   // dir of that module relative to repo root ("." for single-module)

    // ---- task (what the agent is shown) ----
    ProblemStatement string `json:"problem_statement"` // issue text, SYMPTOM-ONLY: fix hints + PR refs scrubbed
    HintsText        string `json:"hints_text"`        // extra context kept SEPARATE; never fed by default

    // ---- oracle (held-out; paths + names only, never bodies) ----
    OracleFiles []string     `json:"oracle_files"`  // *_test.go paths extracted from GOLD at mining time (gotcha #8b)
    FailToPass  []OracleTest `json:"FAIL_TO_PASS"`  // tests that must flip red->green, each PACKAGE-QUALIFIED + -run regex
    PassToPass  PassToPass `json:"PASS_TO_PASS"`  // packages that must STAY green (single meaning — packages only)
    Exec        ExecSpec   `json:"exec"`          // how to run tests hermetically (replaces the old bare test_cmd)
    TestTimeout string     `json:"test_timeout"`  // per-run cap, e.g. "10m" (time.Duration string)

    // ---- validation metadata (the receipt the validator writes) ----
    Validation Validation `json:"validation"`

    // ---- provenance ----
    MinedBy     string   `json:"mined_by"`          // miner version / run id
    ValidatedAt string   `json:"validated_at"`      // RFC3339 timestamp
    LicenseNote string   `json:"license_note"`      // upstream repo SPDX id at base_commit, e.g. "Apache-2.0"
    Aliases     []string `json:"aliases,omitempty"` // prior instance_ids this instance was known by (e.g. legacy "opa-8781")
}

// TestID is the package-qualified IDENTITY of a Go test within the module. Bare
// test names are NOT unique (the same TestFoo can exist in many packages, and
// subtests share a parent name), so every oracle reference and every "which
// tests ran" proof is package-qualified. (O1) TestID carries identity ONLY — no
// run_regex — so it is the exact shape recorded in ran_tests receipts. (O12)
type TestID struct {
    Package string `json:"package"`           // import path relative to module, e.g. "./v1/topdown"
    Name    string `json:"name"`              // top-level test func, e.g. "TestTopDownPartialEvalNegation"
    Subtest string `json:"subtest,omitempty"` // t.Run path if the oracle targets a subtest, e.g. "negation/nested"
}

// OracleTest is a FAIL_TO_PASS entry: a TestID plus the exact anchored -run
// regex that selects it. run_regex lives ONLY on oracle entries, never on a
// ran_tests proof (O12) — a receipt records what identity ran, not how it was
// selected.
type OracleTest struct {
    TestID
    RunRegex string `json:"run_regex"` // exact -run value selecting EXACTLY this test/subtest, anchored ^...$
}

// PassToPass is PACKAGES ONLY — a single unambiguous meaning (O3). Per-test P2P
// pinning is deliberately out of scope for schema v1; if P2P package-level
// flakiness later forces per-test granularity, add a `tests []TestID` field in
// a v2 bump rather than overloading `packages`.
type PassToPass struct {
    Packages []string `json:"packages"` // import paths that must stay green, e.g. ["./v1/topdown"]
}

// ExecSpec pins hermetic execution so every grader runs tests identically (O2).
// There is NO shell: the grader execs argv directly. The grader OWNS the -run
// filter, the package argument, and the -tags flag; the instance never carries
// any of them. Exact, deterministic command composition:
//
//   F2P: for each DISTINCT package among the FAIL_TO_PASS OracleTests, run ONE exec:
//        argv ++ [tagsArg?] ++ [package] ++ ["-run", combinedRegex]
//        where combinedRegex = "^(" + strings.Join(sortedRunRegexBodies,"|") + ")$"
//        over that package's OracleTests (each RunRegex's ^...$ anchors stripped,
//        bodies sorted for determinism). ran_tests MUST prove EVERY listed
//        OracleTest in that package executed (O9). One F2P entry per package is the
//        common case and reduces to the single-alternative regex.
//   P2P: for each package in PASS_TO_PASS.packages, run: argv ++ [tagsArg?] ++ [package]
//   tagsArg = "-tags=" + strings.Join(BuildTags, ",")  (omitted if BuildTags empty)
//
// argv therefore must NOT itself contain -run, -tags, or a package pattern;
// validation rejects an argv that does (analogous prohibition for all three).
// This keeps every grading decision grader-owned and immune to shell-quoting drift.
type ExecSpec struct {
    Cwd       string   `json:"cwd"`                  // dir to run in, relative to repo root (usually == module_dir)
    Argv      []string `json:"argv"`                 // base command, e.g. ["go","test","-count=1","-v"] — no -run/-tags/pkg
    Env       []string `json:"env,omitempty"`        // extra "KEY=VALUE" pairs on top of the hermetic base env
    BuildTags []string `json:"build_tags,omitempty"` // grader appends as -tags=a,b before the package arg
}

// Validation is the determinism receipt (§1.2 / Phase 3). CARDINALITY INVARIANT
// (O5): len(RedAtBaseRuns) == len(GoldGreenRuns) == FlakeRuns exactly — a reader
// that sees a mismatch treats the instance as INVALID, not as an abbreviated
// example. Every run is recorded; never a single boolean.
type Validation struct {
    RedAtBaseRuns []RunResult `json:"red_at_base_runs"` // F2P overlaid on base: must RUN and FAIL, FlakeRuns times
    GoldGreenRuns []RunResult `json:"gold_green_runs"`  // F2P + touched-pkg P2P at gold: must pass, FlakeRuns times
    FlakeRuns     int         `json:"flake_runs"`       // K (default 5); any flip within the K runs => rejected
    CreatedAt     string      `json:"created_at"`       // PR MERGE date (RFC3339) — the contamination knob
    LeakScreen    LeakScreen  `json:"leak_screen"`      // solution-leak screen on problem_statement (binding, R0.1)
    ValidatorVer  string      `json:"validator_version"`
}

type RunResult struct {
    Passed    bool     `json:"passed"`
    RanTests  []TestID `json:"ran_tests"`  // PACKAGE-QUALIFIED proof the named tests ran (guards the -run-miss + testify trap)
    DurationS float64  `json:"duration_s"`
}

// LeakScreen records that problem_statement was checked for fix leakage, to at
// least SWE-bench-Live's standard (n-gram overlap vs the gold diff). Every field
// needed to INDEPENDENTLY RE-RUN the screen and compare across validator
// versions is present (O6): algorithm + params, the exact inputs (by hash) and
// the diff range compared, what was excluded, and the tool commit.
type LeakScreen struct {
    Method        string   `json:"method"`          // e.g. "ngram-overlap-v1"
    NgramSize     int      `json:"ngram_size"`      // n for the n-gram overlap, e.g. 8
    Tokenization  string   `json:"tokenization"`    // e.g. "lowercase-alnum-collapse-ws"
    Passed        bool     `json:"passed"`          // false => candidate rejected (reason: leak)
    Score         float64  `json:"score"`           // measured overlap; below Threshold = pass
    Threshold     float64  `json:"threshold"`
    DiffRange     string   `json:"diff_range"`      // exact range compared, e.g. "base_commit..gold_commit"
    ExcludedFiles []string `json:"excluded_files,omitempty"` // globs excluded from the diff (generated/vendored)
    StatementHash string   `json:"statement_hash"`  // sha256 of the exact problem_statement screened
    GoldDiffHash  string   `json:"gold_diff_hash"`  // sha256 of the diff text compared against
    ToolVersion   string   `json:"tool_version"`    // validator/leak-screen commit SHA
    ScreenedAt    string   `json:"screened_at"`     // RFC3339
}
```

---

## Field semantics (the non-obvious ones)

- **`schema_version`** — required, `"gobench.instance.v1"`. A decoder rejects an
  unknown major. Any breaking change (a new F2P shape, a leak-screen method
  change that alters semantics, a P2P `tests` field) bumps it. (O7)
- **`instance_id`** — `<owner>__<repo>-<pr_number>` (double-underscore separates
  the owner, matching the SWE-bench `owner__repo` escaping so bare repo names
  can't collide across owners). Immutable leaderboard key. `repo_id` (GitHub's
  numeric id) is stored alongside so a repository rename/transfer doesn't orphan
  the instance — resolution falls back to `repo_id` when `repo` 404s. (O8) The
  five hand-built fixtures currently use short ids (`opa-8781`); the Phase 1.3
  migration rewrites them to the canonical form and records the old id as an
  alias.
- **`base_commit` vs `gold_commit`** — precise integration-boundary trees, not
  topological PR parents. See the INVARIANTS comment in the struct. `merge_method`
  is stored because a squash has no PR-commit ancestry and a merge commit has two
  parents; the validator uses it to pick the right pre/post trees and to verify
  the diff actually contains the PR's changes. (O4) The grader never diffs the
  merge object to re-derive oracle files at grade time; the miner extracts
  `oracle_files` from gold once and tags gold in the cache clone immediately
  (gotcha #8b, the shallow-cache lesson). Storing the *paths* is legal
  (metadata); storing the *bodies* is not.
- **`problem_statement`** — symptom-only. Fix hints, PR links, and stack traces
  that name the fix line are scrubbed (Phase 3.7, human-in-the-loop for release
  1, verbatim-minus-hints per the 2026-07-06 issue-text decision). The scrub is
  what `leak_screen` verifies.
- **`oracle_files`** — paths of the held-out `*_test.go` files. The grader
  fetches these from `gold_commit` and overlays them onto the agent's checkout,
  AFTER resetting the agent's own test edits (`git checkout -- '*_test.go'`;
  §1.2). Names, not code, live in the instance.
- **`FAIL_TO_PASS` (`[]OracleTest`)** — package-qualified. Each entry pins the exact
  anchored `-run` regex that selects exactly that test/subtest. A bare name would
  be ambiguous across packages and would let a different package's same-named
  test satisfy the receipt or mask a `-run` miss. The grader runs **one exec per
  distinct F2P package** with a combined alternation `-run` over that package's
  entries, and requires `ran_tests` to prove every listed TestID in the package
  executed (exact rule in the `ExecSpec` composition comment). (O1/O9)
- **`PASS_TO_PASS` (`PassToPass.Packages`)** — packages only, reported separately
  from F2P; the touched packages that must stay green. Single meaning, no
  string/kind ambiguity. (O3)
- **`exec`** — hermetic, shell-free execution. The grader composes the final
  argv; the instance never carries a `-run` or a package pattern. (O2)
- **`ran_tests` (`[]TestID`)** — package-qualified proof the named tests actually
  executed. A `-run` miss exits 0 and reads as a false pass; recording *which
  package-qualified tests ran* defeats both that and the testify
  `TestSuite/Method` prefix trap. (O1/O5)
- **`validation` cardinality** — `len(red_at_base_runs) == len(gold_green_runs)
  == flake_runs`, enforced. (O5)
- **`validation.created_at`** — the PR **merge** date. This is the contamination
  bound: only instances whose gold merged after a release's declared cutoff enter
  that release (GOBENCH.md operating rule 4). It is metadata, not a claim.
- **`validation.leak_screen`** — binding per the G0 adjudication, and now fully
  reproducible: `{method, ngram_size, tokenization, diff_range, excluded_files,
  statement_hash, gold_diff_hash, tool_version}` let anyone re-run the exact
  screen and diff results across validator versions. (O6) If the rolling
  contamination bound is matched by SWE-bench-Live, this screen is what keeps
  axis #1 defensible.
- **absent fields** — there is deliberately no `patch`, no `test_patch`, no
  `hints`-with-fix. If a future consumer wants the gold diff it fetches
  `base..gold` from upstream itself; GoBench does not ship it.

---

## Reproducibility & the fetch-live tradeoff (why metadata-only is still hermetic)

Fetching upstream at grade time trades shipped code for a dependency on upstream
availability. That dependency is bounded, not open-ended:

- **Immutable pins.** `base_commit`/`gold_commit`/`oracle_files` reference commit
  SHAs, which are content-addressed and cannot change under us. A force-push or
  branch delete does not alter a fetched-by-SHA object.
- **Content-verified cache.** The release ships a **content hash manifest** (the
  git object hashes for each pinned commit's tree + each oracle file). The grader
  fetches, then verifies the fetched bytes against the manifest; a mismatch is a
  grader-error signal (`testbuild`-class), never a wrong-answer score. This makes
  "upstream mutated" detectable rather than silent.
- **Cache-first, network-fallback.** Graders read from the local module/commit
  cache first; the network is only the cache-fill path. A frozen benchmark run
  can be replayed fully offline from a warmed cache.
- **Repo-disappearance risk is stated, not waved away.** If an upstream repo is
  deleted (not merely renamed — `repo_id` handles rename), affected instances are
  footnoted on the leaderboard and can be re-pointed at an archived mirror; this
  is the same failure mode SWE-bench-Live carries and is acceptable for a
  metadata-only distribution. The alternative (shipping code) is the thing R0.4
  forbids.

---

## Example (metadata only)

This example uses `flake_runs: 1` so it is a fully literal, valid instance (the
cardinality invariant `len(red_at_base_runs) == len(gold_green_runs) ==
flake_runs` holds exactly). Production default is K=5 — a real instance would
carry five entries in each array.

```json
{
  "schema_version": "gobench.instance.v1",
  "instance_id": "open-policy-agent__opa-8781",
  "repo": "open-policy-agent/opa",
  "repo_id": 66294946,
  "pr_number": 8781,
  "issue_url": "https://github.com/open-policy-agent/opa/issues/8779",
  "base_commit": "966990ccbfb05da5ccbf316fc22be958210c0c83",
  "gold_commit": "3f1c0e9b1a2c4d5e6f7a8b9c0d1e2f3a4b5c6d7e",
  "merge_method": "squash",
  "go_version": "1.22",
  "module_path": "github.com/open-policy-agent/opa",
  "module_dir": ".",
  "problem_statement": "Partial evaluation of a negated expression returns an incorrect result when …",
  "hints_text": "",
  "oracle_files": ["v1/topdown/topdown_partial_test.go"],
  "FAIL_TO_PASS": [
    {"package": "./v1/topdown", "name": "TestTopDownPartialEvalNegation", "run_regex": "^TestTopDownPartialEvalNegation$"}
  ],
  "PASS_TO_PASS": {"packages": ["./v1/topdown"]},
  "exec": {"cwd": ".", "argv": ["go", "test", "-count=1", "-v"]},
  "test_timeout": "10m",
  "validation": {
    "red_at_base_runs": [
      {"passed": false, "ran_tests": [{"package": "./v1/topdown", "name": "TestTopDownPartialEvalNegation"}], "duration_s": 42.1}
    ],
    "gold_green_runs": [
      {"passed": true, "ran_tests": [{"package": "./v1/topdown", "name": "TestTopDownPartialEvalNegation"}], "duration_s": 44.6}
    ],
    "flake_runs": 1,
    "created_at": "2026-05-14T09:22:01Z",
    "leak_screen": {
      "method": "ngram-overlap-v1", "ngram_size": 8, "tokenization": "lowercase-alnum-collapse-ws",
      "passed": true, "score": 0.04, "threshold": 0.25,
      "diff_range": "base_commit..gold_commit", "excluded_files": ["**/*_generated.go", "vendor/**"],
      "statement_hash": "sha256:1b2c…", "gold_diff_hash": "sha256:9f8e…",
      "tool_version": "gobench-validate@c1fe9ab", "screened_at": "2026-07-06T12:00:00Z"
    },
    "validator_version": "gobench-validate/0.1.0"
  },
  "mined_by": "gobench-mine/0.1.0",
  "validated_at": "2026-07-06T12:05:00Z",
  "license_note": "Apache-2.0"
}
```

The five existing hand-built instances (their migrated fixtures ship in
`eval/suite/gobench/testdata/instances/`) are the round-trip
fixtures for Gate G1: they must parse into this schema and reproduce their
recorded verdicts through the §1.2 grader exactly. Their current `meta.json`
carries a subset (`repo`, `base_sha`, `module_dir`, `verify_cmd`, `test_pkg`,
`fail_to_pass`) — Phase 1.3 writes a one-time migration that maps those onto this
superset: derive `gold_commit`/`oracle_files` from the recorded oracle dirs,
package-qualify the `fail_to_pass` name against `test_pkg`, split `verify_cmd`
into `exec.argv`, and rewrite the short `instance_id` to the canonical
`owner__repo-pr` form (old id kept as an alias).

---

## Resolved design questions (were open in draft v0; closed by this revision)

1. **`test_cmd` string → `exec` (argv/cwd/env/build_tags), grader-owned `-run`.**
   A bare shell string was underspecified (shell vs exec, cwd, filter append
   order, quoting). Structured exec + grader-owned filter composition removes the
   ambiguity. (O2)
2. **`PASS_TO_PASS` → packages only.** Dropped the "package OR named subset"
   overload; per-test P2P deferred to a v2 field if ever needed. (O3)
3. **F2P/`ran_tests` → package-qualified `TestID`.** Bare names are not unique in
   Go. (O1)
4. **`base_commit`/`gold_commit` → integration-boundary invariants + `merge_method`.**
   Handles merge/squash/rebase; validator verifies the diff. (O4)

## Still open — deliberately deferred, not defects

- **Leak-screen threshold (`0.25`) is a placeholder.** Calibrate against the 5
  hand-built instances plus a labeled leak/no-leak set in Phase 3; the *fields*
  to record the calibrated value are already in the schema, so tuning the number
  is not a schema change. (was open-Q4)
- **Monorepo multi-module attribution.** v1 assumes one touched module per
  instance (the Phase 2 static filter rejects cross-module PRs), so a single
  `module_dir`/`module_path` suffices. If dolt/vitess yield forces multi-module
  instances, that is a v2 `modules []Module` bump — flagged, not silently
  assumed. (was open-Q3)
