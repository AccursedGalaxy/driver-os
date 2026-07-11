# PROFILES — unified trust + execution profiles

Status: SPEC, council-grilled 2026-07-10 (grill-with-council, critic
`openai/gpt-5.6-sol`, consult run `20260710-101650-c634ff`, 6 rounds; builds
on decision run `20260710-094837-ce9409` = docs/specs/REVIEW-TRIAGE-2026-07-10.md
R1+R2). SHIPPED: S1 recording, S2a/S2b trust layer, and the S3
execution-profile surfaces recorded in §6. OPEN: S4 routing-policy artifact,
S5 TOML user profiles, and §7/S6 requested-vs-resolved run specification
(drafted 2026-07-11 from external review round 3 — NOT yet council-grilled).
Open items needing Robin are marked ⚑.

## Problem

cmd/agent has 61 flags; agent.Config is one flat policy object. "driver-os +
model X" no longer names a reproducible configuration, and the safety posture
(sandbox/local, IsolationNone, no per-action approval) is assembled from
independent flags with an unrestricted-host-shell ambient default. Trust and
reproducibility are two faces of the same fix: name the configuration, make
safety a floor that composition cannot weaken.

## 1. The trust contract

Four trust profiles, canonical names used verbatim in CLI, config,
transcripts, and docs — no aliases:

| profile | sandbox | min isolation | worktree | approval | secrets | network |
|---|---|---|---|---|---|---|
| `trusted-local` | local | none | auto (today's) | none | ambient | unrestricted |
| `reviewed-local` | gated(local) | none | auto (amended; headless tightens to required) | REQUIRED (see §1.2) | clean-env allowlist | unrestricted |
| `container` | docker | process | required | none (amended, see below) | clean-env allowlist | **off** (Robin 2026-07-10) |
| `untrusted` | docker | ≥process | required | policy (default-deny) | none | off |

Amendments at S2b (2026-07-10): container network resolved to OFF (docker
sandbox default already is `--network none`, so this costs nothing); container
approval floor amended policy→NONE — command approval INSIDE a container adds
friction without a security boundary (the containment is the control), and a
gated container would cripple solver runs whose whole point is free execution
inside the boundary. `untrusted` keeps default-deny gating ON TOP of
containment (look-but-barely-touch posture).

**Floor fields — AMENDED to six** (supersedes the five-field set in
REVIEW-TRIAGE R1; the sixth was added on council advice — network-off cannot
be a guarantee if it lives in an overridable execution setting): sandbox
class, min isolation, worktree requirement, approval requirement, secret
exposure, **network policy** (ordering: unrestricted > allowlisted > off).

**Non-weakening**: the trust profile sets the floor; execution profiles and
CLI flags may only tighten floor fields. Weakening = hard startup error
naming the field and the offending layer. Floor fields do NOT use naive enum
ranks — each has a declared satisfaction predicate (isolation is ordinal;
approval modes are partially ordered — `interactive` and `policy` are
incomparable in general; secret exposure is set-inclusion: none ⊂ allowlist ⊂
ambient; network as above). `approve=never` is defined as DENY-ALL, not
approval-bypass.

### 1.1 Consent mechanics

- Headless `cmd/agent` with no `-trust` REFUSES at startup: stable
  setup-error exit (existing exit-1 class, JSON still emitted), and the
  error ENUMERATES all four profiles with one-line threat-model guidance —
  it must not teach a `-trust trusted-local` copy-paste ritual.
- Refusal happens BEFORE any repository content is read: untrusted repo
  content must not be able to influence trust resolution.
- This invariant is enforced and regression-tested in `internal/headless`.
- Deliberately NO env-var form (council O2: persistent env destroys per-run
  friction; call-site visibility is the feature).
- Interactive TUI (`cmd/driver`) defaults `reviewed-local` with
  approve=interactive; the default resolves to a NAMED profile and is
  recorded as defaulted (see §4) — implicit binary behavior is hidden
  experimental treatment otherwise.
- Migration routes by threat model, not blanket `trusted-local`: eval runs
  over public/untrusted repos move to `container`; delegate.sh (own-repo
  implementation runs) gets `trusted-local` explicitly. Same commit as
  activation.
- Execution profiles may declare `requiredTrust` (a MINIMUM floor,
  dimension-wise) but can never SELECT or satisfy consent — `-trust` stays
  mandatory headless. Incomparable CLI-vs-required combinations are
  REJECTED with the dimension named (no silent joins in v1). Both the
  operator-selected and profile-required trust are recorded; a mixed result
  is never relabeled as a canonical profile unless exactly equal.

### 1.2 reviewed-local mechanics

Every model-initiated execution action traverses `sandbox/gated`. Headless
reviewed-local requires a loaded, validated, DEFAULT-DENY, versioned
approval policy — missing/invalid/unversioned policy is a startup error; a
denied action yields the typed refusal path, never a silent fallback to
unrestricted exec. Policy identity + content hash are recorded. Policy
non-weakening is established via a CLOSED REGISTRY of shipped policies in
v1 (structural policy comparison is deferred). Honest framing everywhere:
reviewed-local is host-visible (files, non-secret env) — it is NOT
containment; docs must not present it as container-equivalent.
The unattended policy currently selected by both headless profiles is
`reviewed-local-readonly-v2`. Authorization parses the `run` tool's `sh -c`
source with `mvdan.cc/sh/v3/syntax` and accepts exactly one simple command;
it does not authorize text by prefix. The complete accepted command families
are `go test`, `go build`, and `go vet` with workspace-relative package
arguments and the small compiled-in safe-flag set, plus `git status` and
`git diff` with their compiled-in read-only flags and workspace-relative
pathspecs after `--`. `cat` and `ls` are deliberately absent because fenced
file tools provide workspace reads.

All other syntax is denied before the underlying sandbox executes: assignments,
expansions (including parameter, command, arithmetic, process, tilde, pathname,
and brace expansion), backticks, redirections, pipelines, lists, background or
negated commands, comments, multiple statements/newlines, and unsupported AST
nodes. Absolute paths and cleaned paths that escape via `..` are denied. The
shell remains transport only after this validation; this is a narrow structural
contract, not a claim that shell execution generally is safe. Allowed Go tests
still execute repository code on the host under `reviewed-local`, consistent
with its host-visible, non-containment threat model. The vulnerable
`reviewed-local-readonly-v1` identity remains in the closed registry so old
records are intelligible, but it is retired and cannot authorize execution.
Policy names, versions, canonical hashes, and default-deny behavior remain part
of the recorded contract.


## 2. Execution profiles

Versioned, immutable, compiled-in built-ins first (`coding-v1`,
`interactive-v1`, `eval-swe-v1`). Rules:

- A built-in declares EVERY resolved field explicitly — no inheriting
  mutable package defaults (defaults drifting under a pinned name is silent
  treatment change).
- ANY descriptor change that can alter normalized effective configuration
  ships as a new version name. No "measurement-relevant" field subset —
  that classification is itself a drift surface. Docs/labels excluded from
  resolution may change freely.
- Old versions are retained indefinitely (revisit only on binary-size
  evidence); a versioned name is NEVER silently redirected.
- Profiles never name model slugs (routing stays in ROUTING.md / §5) and
  never satisfy trust consent (§1.1).
- User-defined TOML profiles (`~/.config/driver-os/profiles/*.toml`) are S5;
  they obey the same floor and are recorded by canonical content hash.

## 3. Resolution seam

One pure entry point: `resolve(trust, execProfile, cliFlags) →
NormalizedConfig + ResolutionTrace`, with internal phases: merge (producing
a provenance-carrying intermediate) → trust-floor enforcement
(field-specific predicates) → validation (extends Config.Validate).
Implementation notes:

- Floor fields live in ONE ordered, typed descriptor table (extraction +
  predicate + provenance formatting per field), not a bare map.
- Completeness is anchored OUTSIDE the table: trust-relevant Config fields
  carry a struct-tag annotation, and a conformance test cross-checks tags ↔
  table (a test generated from the table itself is tautological).
- The resolver is library code in its own package (owner: `internal/profile`
  or similar — not cmd/*, not agent, so neither CLI nor runtime becomes the
  dependency root ⚑ final package name at implementation).
- Both binaries call the same resolver. Binary unification is deliberately
  DECOUPLED: it is UX/packaging work, revisited after S3 when compatibility
  requirements are known (⚑ Robin's call, unchanged from CLI phase 3).

## 4. Recording (reproducibility)

Transcript records: profile name+version (nullable until S2/S3),
operator-selected trust + profile-required trust, canonical normalized
config (versioned serialization, secret-redaction rules fixed at S1),
per-field provenance/override trace, profile descriptor content hash,
harness commit + dirty marker, prompt sha256, tool-schema sha256, binary +
invocation-surface identity, approval-policy id+hash (reviewed-local),
routing-policy version+hash (S4). Report carries hashes + names; full
config lives in the transcript.

## 5. Routing-policy artifact (S4)

Decision (council-moved from status-quo-b to c): a separate, independently
versioned, machine-readable routing-policy artifact maps model slugs →
policy class and declares minimum harness policy (e.g. cheap ⇒ reviewer
REQUIRED; flagship-no-reviewer stays a default, never a prohibition).
Resolution combines it monotonically with profile + CLI (stricter wins).
Unknown slugs conservatively require review; waivers are explicit and
recorded. The structured artifact becomes canonical and the ROUTING.md
table is GENERATED from it (extends the existing cmd/routing direction).
⚑ Interim: status quo (orchestrator responsibility per skill/docs) is
acceptable ONLY time-bounded — gate: once S4 ships, reproducibility-grade
eval campaigns REFUSE to run without a versioned routing policy; Robin to
confirm the milestone.

Eval campaigns are NOT execution profiles: a checked-in, versioned, hashed
CAMPAIGN MANIFEST references an execution profile and declares
dataset/model/prompt variants + overrides. CLI overrides on a campaign run
mark it noncanonical in the transcript.

## 6. Slices

- **S1 — recording (SHIPPED)** (zero behavior change): versioned canonical
  serialization of TODAY'S effective config + hashes (prompt, tool-schema,
  config), binary/invocation identity, dirty-build marker. Profile fields
  reserved/nullable. Ships alone; immediate reproducibility win.
- **S2 — trust layer (SHIPPED)** (two sub-slices, atomic activation):
  S2a landed the resolver package, descriptor table, predicates, conformance
  tests, and gated reviewed-local mechanics. S2b atomically flipped
  fail-closed headless and updated in-repo call sites (delegate.sh →
  `-trust trusted-local`; eval-on-public-repos → `container`) without a
  bypassable intermediate release.
- **S3 — execution profiles (SHIPPED)**: four active profiles (`coding-v2`,
  `interactive-v2`, `observe-v1`, and `eval-swe-v1`; frozen v1 compatibility
  profiles remain available), non-safety precedence (CLI > profile > default),
  and full override provenance. The pure resolver is consumed by ordinary,
  best-of, ladder, TUI, and eval construction paths. ConfigRecord v7 records
  required trust, canonicality, and complete field provenance; canonical means
  no profile-resolved field has CLI source.
- **S4 — routing-policy artifact + campaign manifests** (spec may proceed in
  parallel earlier; binds to S3 identities).
- **S5 — TOML user profiles; subcommand CLI rides the separate unification
  decision.**

## 7. Requested vs Resolved run specification (S6 — DRAFT 2026-07-11)

Adopted from external review round 3 (backlog §round-3). The disease: a
run's policy exists in ~10 overlapping representations (CLI strings, profile
declarations, trust floors, flat `agent.Config`, `loopKnobs`,
`TerminationPolicy`, `ConfigRecord`, bundle identity, report schema), and
default-filling happens lazily and REPEATEDLY: `resolveKnobs` runs once per
loop and again inside `effectiveConfig` (configrecord.go:204);
`ReviewRounds <= 0 → default` is applied in three independent places
(review.go, review_pass.go, configrecord.go); `verifyTimeout` in two.
Nothing but purity-by-luck guarantees the executed and recorded values agree.

This is not hypothetical. Verified divergence (2026-07-11): the headless
Config builders (`internal/headless/main.go` `baseCfg` and
`buildAgentConfigInDir`) never thread `AutoVerify`, `AutoVerifySoft`,
`StandingContext`, `NavSpiralWindow`, or `AnswerNudgeWindow` from the
resolved profile, while the TUI and eval paths do. A headless `coding-v2`
run executes with `AutoVerify=false` against a profile that declares `true`,
and the record still stamps the profile identity (the resolved trace says
one thing, execution and record agree on another). The per-binary
hand-assembled `agent.Config{}` literal (6+ sites: headless ×2, TUI, eval,
council ×3, issue-bot, chat) is the structural cause.

### 7.1 The three-way split

`agent.Config` today mixes three different kinds of thing. Separate them:

- **`ResolvedSpec`** — pure policy DATA: every knob the loop, gates,
  detectors, review, and recording consume. Complete (every field explicitly
  set — zero is a value, never "use default"), validated, immutable after
  construction, serializable. Carries per-field provenance (the existing
  `FieldProvenance` machinery, widened to all fields).
- **Runtime bindings** — injected dependencies: `Model`, `Sandbox`,
  `VerifySandbox`, `Memory`, `Tools`, `Obs`, `PerfMark`, `Spend`, `CostFn`.
  Never serialized; identity (model slugs, sandbox kind) lives in the spec.
- **Content** — `Task`, `TaskImages`, `History`, `Root`. Per-run payload,
  recorded separately (task hash already in bundle identity).

### 7.2 Resolution happens once

One eager constructor, extending the existing §3 seam rather than adding a
parallel one:

    runspec.Resolve(RequestedConfig) → (ResolvedSpec, Trace, error)

- `RequestedConfig` = trust selection + exec-profile name + explicit CLI
  overrides + CLI-only fields, preserving UNSET state (the §3 resolver's
  `Overrides` shape, widened from ~19 profile fields to the full policy
  surface).
- Resolution order (unchanged from §3): profile declarations → CLI
  overrides → trust-floor predicates → compatibility aliases → validation.
  Contradictions are startup errors, before any sandbox or provider session
  exists.
- ALL `<= 0 means default` filling moves here. `resolveKnobs`,
  the default-filling half of `resolveTerminationPolicy`, and the scattered
  `ReviewRounds`/`verifyTimeout` fallbacks are deleted from loop/record
  code; the loop reads spec fields directly and may assert completeness,
  never repair it.
- `NavSpiralWindow` dual-source collapses: the standalone field becomes a
  resolve-time alias into `TerminationPolicy.NavSpiralWindow` (standalone
  wins, provenance says so), and only the policy value exists downstream.
- One shared builder replaces the 6+ hand-assembled `agent.Config{}`
  literals. Internal callers (council, chat, issue-bot) go through a
  minimal builder entry with a named internal profile — no bare literals.

### 7.3 Record = the spec, not a re-derivation

`ConfigRecord`'s `EffectiveConfig` is replaced by the canonical
serialization of the `ResolvedSpec` VALUE that the loop actually held —
`effectiveConfig()` (and its second `resolveKnobs` call) is deleted.
`ConfigSHA256` hashes that serialization. Bundle identity and report fields
derive from the same value. There is exactly one place a policy number can
come from.

Values derived DURING the run (e.g. `resolveAutoVerify`'s harness-derived
`VerifyCmd`) are not config and must not mutate the spec: they are recorded
as runtime-derived evidence (the existing `autoVerifyResolved` /
derived-provenance channel), with the spec keeping the pre-run value. Fixes
the current silent mid-run `cfg` copy mutation (loop_shared.go:153).

### 7.4 Conformance (the anti-drift teeth)

No hand-maintained parallel structs with field copying. Two reflection
oracles, extending the existing `configFieldClasses` mechanism:

1. **Consumed-and-recorded-exactly-once**: every `ResolvedSpec` field is
   classified (loop-consumed / gate-consumed / record-only / excluded-with-
   reason) and appears in the canonical serialization exactly once. A new
   field fails the test until classified.
2. **Profile-threading completeness**: every `profile.FieldID` declared by
   any registered exec profile provably lands in `ResolvedSpec` (this test
   alone would have caught the headless AutoVerify drop).

### 7.5 Slices

- **S6a — widen the resolver + shared builder, zero behavior change.**
  `ResolvedSpec` introduced; all binaries construct through it; golden test:
  byte-identical `ConfigRecord` across a matrix of trust × profile × CLI
  combinations vs the pre-S6a binary. EXCEPTION carved out explicitly: the
  headless profile-field drop (above) — preserving it byte-identical means
  reproducing a bug, so S6a either replicates it behind a named compat
  shim or takes the behavior change consciously (⚑ below).
- **S6b — delete lazy defaults from loop/record code.** `resolveKnobs`
  fallbacks, `effectiveConfig`, triple `ReviewRounds` default, dual
  `verifyTimeout`, `NavSpiralWindow` standalone field. Loop signature takes
  the spec; `<= 0` in a spec field is a validation error, not a default.
- **S6c — conformance oracles** (7.4), plus schema bump for the record
  (EffectiveConfig → canonical ResolvedSpec serialization).

Interaction with CONTROL-PLANE Design 1 (loop unification): independent and
prior — a single `ResolvedSpec` shrinks both loops' shared wiring and makes
the eventual unification diff smaller. Do S6 first; do not couple them.

## Open for Robin ⚑

6. **Headless `AutoVerify` semantics (S6a gate)**: fixing the profile-field
   drop flips headless `coding-v2` runs to `AutoVerify=true` (soft
   auto-derived verify gate when `-verify-cmd` is empty) — a behavior change
   for every delegation run that omits `-verify-cmd`. Take the change as
   intended-profile-semantics, or amend `coding-v2`→`coding-v3` with
   `AutoVerify=false` for headless parity? Same question for
   `StandingContext`/`AnswerNudgeWindow`/`NavSpiralWindow` threading.

## Older open items ⚑

1. Binary unification (unchanged decision point, now explicitly decoupled).
2. ~~Container network default~~ — RESOLVED 2026-07-10: off.
3. S4 time-bound: confirm "reproducibility-grade campaigns refuse without
   versioned routing policy" as the gate, or set a date.
4. ~~Resolver package name~~ — RESOLVED: `internal/profile` (shipped S2a).
5. ~~TUI default vs worktree floor~~ — RESOLVED at S3 (ffa60cb): the
   reviewed-local worktree FLOOR amended required→auto; interactive
   per-command approval substitutes for workspace isolation as the review
   mechanism; headless reviewed-local keeps forcing a worktree as a
   tightening. TUI defaults reviewed-local (approve interactive, clean
   env); `-trust trusted-local` restores the previous TUI behavior.
