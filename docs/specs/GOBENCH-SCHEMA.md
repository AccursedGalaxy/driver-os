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
   construction** here — that is the point, not an omission.
2. **The problem statement carries a leak-screen receipt.** The R0.1 audit
   (`GOBENCH-PRIOR-ART.md`) found the rolling/contamination story is no longer
   unique (SWE-bench-Live does monthly n-gram leak screening). So a
   `problem_statement` that leaks the fix silently degrades the whole benchmark.
   Every instance records a `leak_screen` receipt; the validator (Phase 3) MUST
   populate it, executed at least to SWE-bench-Live's standard.

Port-compatibility: where a field also exists in `swebench.Instance` /
`swerebench.Instance` (`eval/suite/swebench/swebench.go`,
`eval/suite/swerebench/swerebench.go`), GoBench reuses the **exact JSON key**
(incl. the upstream `FAIL_TO_PASS` / `PASS_TO_PASS` capitalization) so existing
decoders and the `SWEInstance()` bridge keep working.

---

## The Go struct

Lives at `eval/suite/gobench/instance.go` (Phase 1.3). Field order below groups
by role; JSON keys are the contract.

```go
// Instance is one GoBench task: a single real Go bug, mined from a merged PR,
// validated red-at-base / gold-green, distributed as metadata only (no gold
// source or test code — the grader fetches upstream at grade time).
type Instance struct {
    // ---- identity ----
    InstanceID string `json:"instance_id"` // "<repo-slug>-<pr_number>", e.g. "opa-8781"
    Repo       string `json:"repo"`        // "open-policy-agent/opa"
    PRNumber   int    `json:"pr_number"`   // the gold PR
    IssueURL   string `json:"issue_url"`   // the linked issue the PR closed

    // ---- commits (the grader checks these out; no code is embedded) ----
    BaseCommit string `json:"base_commit"` // PR parent — the red state the agent starts from
    GoldCommit string `json:"gold_commit"` // PR merge — the green state; oracle files come from here
    GoVersion  string `json:"go_version"`  // from go.mod at base, e.g. "1.22"
    ModulePath string `json:"module_path"` // go module import path of the touched module
    ModuleDir  string `json:"module_dir"`  // dir of that module relative to repo root ("." for single-module)

    // ---- task (what the agent is shown) ----
    ProblemStatement string `json:"problem_statement"` // issue text, SYMPTOM-ONLY: fix hints + PR refs scrubbed
    HintsText        string `json:"hints_text"`        // extra context kept SEPARATE; never fed by default

    // ---- oracle (held-out; paths + names only, never bodies) ----
    OracleFiles []string `json:"oracle_files"` // *_test.go paths extracted from GOLD at mining time (gotcha #8b)
    FailToPass  TestList `json:"FAIL_TO_PASS"`  // test names that must flip red->green, each with a verified -run regex
    PassToPass  TestList `json:"PASS_TO_PASS"`  // package list / named subset that must STAY green
    TestCmd     string   `json:"test_cmd"`      // the exact go-test invocation, e.g. "go test ./v1/topdown/..."
    TestTimeout string   `json:"test_timeout"`  // per-run cap, e.g. "10m" (time.Duration string)

    // ---- validation metadata (the receipt the validator writes) ----
    Validation Validation `json:"validation"`

    // ---- provenance ----
    MinedBy     string `json:"mined_by"`     // miner version / run id
    ValidatedAt string `json:"validated_at"` // RFC3339 timestamp
    LicenseNote string `json:"license_note"` // upstream repo SPDX id at base_commit, e.g. "Apache-2.0"
}

// TestList decodes both a JSON array and a JSON-encoded string of names
// (SWE-bench Lite ships the latter) — reuse eval/suite/swerebench TestList.
type TestList []string

// Validation is the determinism receipt (§1.2 / Phase 3). N runs each, all
// recorded — never a single boolean.
type Validation struct {
    RedAtBaseRuns  []RunResult `json:"red_at_base_runs"`  // F2P overlaid on base: must RUN and FAIL, K times
    GoldGreenRuns  []RunResult `json:"gold_green_runs"`   // F2P + touched-pkg P2P at gold: must pass, K times
    FlakeRuns      int         `json:"flake_runs"`        // K (default 5); any flip within the K runs => rejected
    CreatedAt      string      `json:"created_at"`        // PR MERGE date (RFC3339) — the contamination knob
    LeakScreen     LeakScreen  `json:"leak_screen"`       // solution-leak screen on problem_statement (binding, R0.1)
    ValidatorVer   string      `json:"validator_version"`
}

type RunResult struct {
    Passed    bool     `json:"passed"`
    RanTests  []string `json:"ran_tests"`  // names PROVEN to have run (guards the testify TestSuite/Method -run trap)
    DurationS float64  `json:"duration_s"`
}

// LeakScreen records that problem_statement was checked for fix leakage, to at
// least SWE-bench-Live's standard (n-gram overlap vs the gold diff). Populated
// by the validator; a fail here rejects the candidate (reason: leak).
type LeakScreen struct {
    Method    string  `json:"method"`     // e.g. "ngram-overlap-v1"
    Passed    bool    `json:"passed"`     // false => candidate rejected
    Score     float64 `json:"score"`      // measured overlap; below threshold = pass
    Threshold float64 `json:"threshold"`
    ScreenedAt string `json:"screened_at"`
}
```

---

## Field semantics (the non-obvious ones)

- **`instance_id`** — `<repo-slug>-<pr_number>` matches the existing hand-built
  bank (`opa-8781`, `dolt-11215`, …). Stable across releases; the leaderboard
  keys on it.
- **`base_commit` vs `gold_commit`** — base is the PR *parent* (red), gold is the
  *merge* (green). The grader never diffs the merge object to re-derive oracle
  files at grade time; the miner extracts `oracle_files` from gold once and tags
  gold in the cache clone immediately (GOBENCH.md gotcha #8b, the shallow-cache
  lesson). Storing the *paths* is legal (metadata); storing the *bodies* is not.
- **`problem_statement`** — symptom-only. Fix hints, PR links, and stack traces
  that name the fix line are scrubbed (Phase 3.7, human-in-the-loop for release
  1). The scrub is what `leak_screen` verifies.
- **`oracle_files`** — paths of the held-out `*_test.go` files. The grader
  fetches these from `gold_commit` and overlays them onto the agent's checkout,
  AFTER resetting the agent's own test edits (`git checkout -- '*_test.go'`;
  §1.2). Names, not code, live in the instance.
- **`FAIL_TO_PASS`** — each entry pins the exact `-run` regex. A `-run` miss
  exits 0 and reads as a false pass, so the validator records `ran_tests` proving
  the named test actually executed (the testify `TestSuite/Method` prefix trap).
- **`PASS_TO_PASS`** — reported separately from F2P; a package list or named
  subset of the touched packages that must stay green.
- **`validation.created_at`** — the PR **merge** date. This is the contamination
  bound: only instances whose gold merged after a release's declared cutoff enter
  that release (GOBENCH.md operating rule 4). It is metadata, not a claim.
- **`validation.leak_screen`** — binding per the G0 adjudication. If the rolling
  contamination bound is matched by SWE-bench-Live, the leak screen is what keeps
  axis #1 defensible; without it the whole differentiation weight falls on the
  honesty axis.
- **absent fields** — there is deliberately no `patch`, no `test_patch`, no
  `hints`-with-fix. If a future consumer wants the gold diff it fetches
  `base..gold` from upstream itself; GoBench does not ship it.

---

## Example (metadata only)

```json
{
  "instance_id": "opa-8781",
  "repo": "open-policy-agent/opa",
  "pr_number": 8781,
  "issue_url": "https://github.com/open-policy-agent/opa/issues/8779",
  "base_commit": "966990ccbfb05da5ccbf316fc22be958210c0c83",
  "gold_commit": "3f1c0e9b…",
  "go_version": "1.22",
  "module_path": "github.com/open-policy-agent/opa",
  "module_dir": ".",
  "problem_statement": "Partial evaluation of a negated expression returns an incorrect result when …",
  "hints_text": "",
  "oracle_files": ["v1/topdown/topdown_partial_test.go"],
  "FAIL_TO_PASS": ["TestTopDownPartialEvalNegation"],
  "PASS_TO_PASS": ["./v1/topdown"],
  "test_cmd": "go test ./v1/topdown/...",
  "test_timeout": "10m",
  "validation": {
    "red_at_base_runs": [{"passed": false, "ran_tests": ["TestTopDownPartialEvalNegation"], "duration_s": 42.1}],
    "gold_green_runs":  [{"passed": true,  "ran_tests": ["TestTopDownPartialEvalNegation"], "duration_s": 44.6}],
    "flake_runs": 5,
    "created_at": "2026-05-14T09:22:01Z",
    "leak_screen": {"method": "ngram-overlap-v1", "passed": true, "score": 0.04, "threshold": 0.25, "screened_at": "2026-07-06T12:00:00Z"},
    "validator_version": "gobench-validate/0.1.0"
  },
  "mined_by": "gobench-mine/0.1.0",
  "validated_at": "2026-07-06T12:05:00Z",
  "license_note": "Apache-2.0"
}
```

The five existing hand-built instances
(`docs/findings/harness-bench/swe-instances/*/meta.json`) are the round-trip
fixtures for Gate G1: they must parse into this schema and reproduce their
recorded verdicts through the §1.2 grader exactly. Their current `meta.json`
carries a subset (`repo`, `base_sha`, `module_dir`, `verify_cmd`, `test_pkg`,
`fail_to_pass`) — Phase 1.3 writes a one-time migration that maps those onto this
superset (deriving `gold_commit`/`oracle_files` from the recorded oracle dirs).

---

## Open questions for the council review (Gate G1)

1. **`test_cmd` generality vs a `version`-keyed command table.** SWE-bench keys
   test commands to 12 Lite repos via `version`; GoBench spans ~54 repos
   (`eval/suite/gobench/repos.yaml`). Store the literal `test_cmd` per instance
   (chosen above, simpler, self-describing) or a `version` key + per-repo table
   (less redundant, but a new indirection)? Draft picks literal.
2. **`PASS_TO_PASS` granularity.** Package list (cheap, coarse) vs named-test
   subset (precise, heavier to mine). Draft picks package list; revisit if P2P
   flakiness forces per-test pinning.
3. **`oracle_files` multi-module repos.** Does a single `module_dir` suffice, or
   do we need per-file module attribution for monorepos (dolt/vitess)? Draft
   assumes one touched module per instance (the miner rejects cross-module PRs,
   Phase 2 static filter).
4. **Leak-screen threshold.** `0.25` n-gram overlap is a placeholder; calibrate
   against the 5 hand-built instances + a labeled leak/no-leak set in Phase 3.
