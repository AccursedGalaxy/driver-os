# PROFILES — unified trust + execution profiles

> Some links below (review-triage specs, `eval/` build files) live in the
> private companion repository and are not published here.

Status: SPEC, council-grilled 2026-07-10 (grill-with-council, critic
`openai/gpt-5.6-sol`, consult run `20260710-101650-c634ff`, 6 rounds; builds
on decision run `20260710-094837-ce9409` = docs/specs/REVIEW-TRIAGE-2026-07-10.md
R1+R2). SHIPPED: S1 recording, S2a/S2b trust layer, and the S3
execution-profile surfaces recorded in §6. OPEN: S4 routing-policy artifact,
S5 TOML user profiles, and §7/S6 requested-vs-resolved run specification
(drafted 2026-07-11 from external review round 3; council-grilled same day —
critic `openai/gpt-5.6-sol`, run `20260711-153523-5f88a4`, 4 rounds,
hit_cap with signal 1.00: 13 objections, 11 critic/referee-closed, final 2
(O12 append-only runtime-resolution sequence, O13 S6a behavioral ship gate)
folded in without a re-read). Open items needing Robin are marked ⚑.

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
- Profiles never name model slugs (routing lives in its own table / §5) and
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
recorded. The structured artifact becomes canonical and the routing
table is GENERATED from it (extends the existing generator direction).
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

## 7. Requested vs Resolved run specification (S6 — council-grilled 2026-07-11)

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
  detectors, review, and recording consume. Complete (every field carries an
  explicitly resolved value — unset state exists only in `RequestedConfig`,
  never here), validated per-field (each field has its own valid range and
  disable semantics; see §7.2), and serializable. Per-field provenance (the
  existing `FieldProvenance` machinery, widened to all fields) travels WITH
  the spec but is a separate serialization from the policy value (§7.3).
  Immutability is enforced by ACCESS STRUCTURE, not by convention or
  after-the-fact checking: policy storage lives in unexported fields of the
  spec type (its own package), construction deep-copies all referenced
  storage (slices/maps/pointers, including `TerminationPolicy`), and
  consumers cannot reach mutable state — components receive small
  BY-VALUE per-domain projections (loop knobs, gate config, review config,
  termination policy — a handful of group structs, not ~77 accessors), and
  any accessor returning referenced data returns a copy. The canonical
  policy serialization + hash are captured AT construction; run teardown
  re-serializes and compares as DEFENSE-IN-DEPTH (it detects a bug in the
  copy discipline; it is not the enforcement mechanism, since a mutate-and-
  restore or an already-consumed mutation would evade an end-of-run check).
- **Runtime bindings** — injected dependencies: `Model`, `Sandbox`,
  `VerifySandbox`, `Memory`, `Tools`, `Obs`, `PerfMark`, `Spend`, `CostFn`.
  Never serialized; identity (model slugs, sandbox kind) lives in the spec.
- **Content** — `Task`, `TaskImages`, `History`, `Root`. Per-run payload,
  recorded separately (task hash already in bundle identity).

### 7.2 Resolution happens once

One eager constructor, extending the existing §3 seam rather than adding a
parallel one:

    runspec.Resolve(RequestedConfig) → (ResolvedSpec, Trace, error)

- `RequestedConfig` is TRANSPORT-NEUTRAL: an optional-valued (presence-
  preserving) struct over the FULL policy surface — trust selection,
  exec-profile name, and per-field optional overrides. CLI flag parsing is
  merely one PRODUCER of it; programmatic callers (council, chat,
  issue-bot, tests, future API) construct it directly and may set any
  policy field without needing a named profile to bless the value. A named
  internal profile supplies their DEFAULTS, not their ceiling.
- Resolution order: profile declarations → explicit overrides →
  **compatibility-alias normalization** → trust-floor predicates →
  validation. Aliases normalize BEFORE floor enforcement so no alias can
  alter a policy value after the non-weakening check; an alias and its
  canonical field both explicitly set to conflicting values is a startup
  ERROR, never a silent precedence win. Contradictions are startup errors,
  before any sandbox or provider session exists.
- ALL default filling moves here. `resolveKnobs`, the default-filling half
  of `resolveTerminationPolicy`, and the scattered `ReviewRounds`/
  `verifyTimeout` fallbacks are deleted from loop/record code; the loop
  reads spec fields directly and may assert completeness, never repair it.
  Unset-vs-zero is carried by PRESENCE in `RequestedConfig` (option types),
  not by sentinel values: each field has its own declared valid range and
  disable semantics (zero legitimately means "disabled" for some windows/
  counts), and resolved-field validation is per-field schema, not a blanket
  `<= 0` rule.
- `NavSpiralWindow` dual-source collapses: the standalone field becomes a
  compatibility alias of `TerminationPolicy.NavSpiralWindow`, normalized in
  the alias phase above (conflict = error), and only the policy value
  exists downstream.
- One shared builder replaces the 6+ hand-assembled `agent.Config{}`
  literals. No bare literals anywhere: every construction path produces a
  `RequestedConfig` and goes through `Resolve`.

### 7.3 Record = the spec, not a re-derivation

`ConfigRecord`'s `EffectiveConfig` is replaced by the canonical
serialization of the `ResolvedSpec` VALUE that the loop actually held —
`effectiveConfig()` (and its second `resolveKnobs` call) is deleted. Two
distinct canonical serializations with distinct hashes:

- **Policy value** → `ConfigSHA256`. Two runs with the same effective
  policy share this hash regardless of HOW each value was reached (profile
  default vs explicit override).
- **Resolution trace** (per-field provenance, requested inputs, alias
  normalizations, canonicality) → recorded alongside under its own field
  and hash (`ResolutionTraceSHA256`). Identity questions ("same policy?")
  and audit questions ("same way of asking for it?") stay separable.

Bundle identity and report fields derive from the same values. There is
exactly one place a policy number can come from.

Values derived DURING the run (e.g. `resolveAutoVerify`'s harness-derived
`VerifyCmd`) are not config and must not mutate the spec — but they DO
control behavior (the derived command executes code), so they get the same
integrity treatment as config, not a loose evidence side-channel: a
canonical **runtime-resolution record** — an APPEND-ONLY, canonically
ordered event sequence, not a singular value (derivation is per-turn and
workspace markers can change mid-run; a single "the selected value" slot
would let overwrites hide temporal divergence). Each entry carries
turn/phase, the derived value, derivation-input digests (which project
markers matched, workspace state consulted), provenance, and the consuming
gate/action; the hash of the complete sequence is included in bundle
identity. The spec keeps the pre-run value; the runtime-resolution
sequence explains the rest. Fixes the current silent mid-run `cfg` copy
mutation (loop_shared.go:153), and closes the gap where two runs sharing
`ConfigSHA256` execute different derived verify commands invisibly.

### 7.4 Conformance (the anti-drift teeth)

No hand-maintained parallel structs with field copying. Three oracles —
two structural (reflection, extending `configFieldClasses`) and one
BEHAVIORAL (structure alone cannot prove a resolved value reaches its
consumption point; a field can serialize exactly once and still be ignored
or shadowed by the loop):

1. **Consumed-and-recorded-exactly-once** (structural): every
   `ResolvedSpec` field is classified (loop-consumed / gate-consumed /
   record-only / excluded-with-reason) and appears in the canonical policy
   serialization exactly once. A new field fails the test until classified.
2. **Profile-threading completeness** (structural): every `profile.FieldID`
   declared by any registered exec profile provably lands in
   `ResolvedSpec`.
3. **Consumption dataflow** (behavioral): a table-driven test varies each
   profile-declared field to a distinguishable non-default value and
   asserts the value observed AT ITS ACTUAL CONSUMPTION SITE, in BOTH
   `Run` and `RunNative`. "Consumption site" is per-field and named in the
   field's classification entry (oracle 1), so the mapping field →
   consuming component is itself recorded and reviewable: the tracker's
   spiral window state for `NavSpiralWindow`, the constructed gate chain
   for fence/verify/review fields, the review pass's round budget for
   `ReviewRounds`, etc. Observation is a test-only hook AT the consuming
   component — not a central startup snapshot, which a loop could populate
   correctly and then ignore. For fields whose consumption is an
   arm/disarm decision (e.g. `AutoVerify`), the assertion is behavioral:
   the derived gate actually appears (or doesn't) in the constructed gate
   chain. A field whose classification names no consumption site cannot be
   classified loop- or gate-consumed. This is the oracle that would have
   caught the headless AutoVerify drop end-to-end; oracle 2 alone only
   proves the resolver's side. Scope: all profile FieldIDs at minimum;
   loop-consumed non-profile fields are added as they migrate in S6b.

### 7.5 Slices

> **STATUS 2026-07-12: S6a, S6b, S6c ALL SHIPPED** (commits 4373ecd,
> 23ff45e, b2db2d3, 05195b3). Ship gate green: behavioral consumption
> oracle over all profile FieldIDs in both loops
> (agent/consumption_oracle_test.go); structural oracles #1/#2 in
> agent/policy_oracle_test.go; record schema v9. One deviation from the
> letter of S6a/S6b, recorded in the work backlog: `agent.Config` survives
> as the sanctioned REQUESTED-SIDE input shape (one canonical converter,
> `Config.Split()` — never seen by the loops) because headless/driver/eval
> still assemble it from their flag layers; the native
> RequestedConfig-from-flags builders (and with them Config's deletion)
> are the owed follow-up — now specified as **S6d** below (specced +
> council-hardened 2026-07-13, not started). S6d turned out to be larger
> than "swap the builders": the same audit found a SECOND unrecorded lane
> (five paths resolve and call a loop directly) and TWO disagreeing
> provenance views inside every ConfigRecord.

- **S6a — full eager resolution + shared builder + loop signature, zero
  behavior change.** The ResolvedSpec invariant holds from day one of S6a:
  `Resolve` performs COMPLETE eager default-filling (exactly once), all
  binaries construct through the shared builder, and BOTH loop signatures
  change in S6a itself to take the split directly —
  `Run(ctx, spec ResolvedSpec, rt Runtime, content Content)` and likewise
  `RunNative` — with NO `agent.Config`-shaped adapter in between (an
  adapter that copies fields is exactly the manual-copy drift class this
  design deletes; if it existed it would need its own totality proof, so it
  doesn't exist). `agent.Config` survives S6a only as an input shape on the
  requested side, consumed by `Resolve`, never seen by the loops. The
  legacy fallbacks inside loop/record code are not yet deleted but are
  PROVEN DEAD in S6a itself: a completeness assertion at loop entry plus a
  test that every fallback branch is unreachable given any `Resolve`
  output (resolution is pure, so the record path re-deriving from the same
  complete value stays byte-identical). No transitional "ResolvedSpec that
  isn't actually complete" exists at any point. Golden test: byte-identical
  `ConfigRecord` across a matrix of trust × profile × CLI combinations vs
  the pre-S6a binary. SHIP GATE: S6a is non-shippable until the
  profile-FieldID portion of the behavioral consumption oracle (§7.4 #3)
  passes for BOTH loops — S6a rewrites every construction path and both
  loop APIs, exactly the area where fields were previously dropped, and
  completeness assertions + record goldens cannot prove consumers USE the
  resolved values; broader non-profile coverage stays in S6c. EXCEPTION
  carved out explicitly: the headless profile-field drop (above) — a
  wiring bug, not a semantics question. S6a honors the declared
  `coding-v2` semantics in ALL binaries (profiles are global contracts; a
  `coding-v3` with `AutoVerify=false` would change every consumer to
  preserve one binary's bug). If migration compatibility is needed for
  in-flight tooling, an explicitly named `legacy-headless-v1` profile
  replicates the dropped-field behavior, opt-in, with a recorded removal
  date (⚑ below: confirm behavior change vs legacy profile).
- **S6b — delete the (now-dead) fallbacks.** Pure removal of the code S6a
  made unreachable: `resolveKnobs` fallbacks, the default-filling half of
  `resolveTerminationPolicy`, `effectiveConfig`, triple `ReviewRounds`
  default, dual `verifyTimeout`, plus the `NavSpiralWindow` standalone
  field. No API changes here — the loop signatures already changed in S6a;
  S6b removes dead branches only. An out-of-range spec field per its own
  declared schema is a validation error, not a default (zero stays valid
  where it means "disabled").
- **S6c — conformance oracles** (§7.4: two structural + the full
  behavioral consumption matrix beyond S6a's profile-FieldID ship gate),
  plus schema bump for the record (EffectiveConfig → canonical
  policy-value + resolution-trace serializations, runtime-resolution
  sequence in bundle identity).
- **S6d — one resolve-and-record seam; delete `agent.Config`**
  (specced 2026-07-13; council-hardened same day — critic `openai/gpt-5.6-sol`,
  run `20260713-005417-54ef57`, 4 rounds, hit_cap with signal ratio 1.00: 13
  objections, all 13 answered, 12 closed by the critic/referee, O13 folded in
  without a re-read. Every round forced a real revision — v0 of this slice was
  wrong in four separate ways, listed under "what the council killed" below.)

  ### Residual state (audited 2026-07-13)

  Four production files still write an `agent.Config` literal:
  `headless/configbuild.go` `buildBaseConfig` (~44 fields), `headless/main.go`
  `buildAgentConfigInDir` (~40), `cmd/driver/configbuild.go`
  `buildBaseAgentConfig` (~38, via `NewSessionFromConfig`),
  `cmd/eval/configbuild.go` `buildEvalConfig` (~24), plus the library riders
  `eval/trial.go` and `eval/suite/councilab`. `agent.Config` has 76 exported
  fields. Test tail: ~22 files with qualified `agent.Config{}`, ~64 in-package
  files with bare `Config{}`.

  Five OTHER paths (council ×3 `observeRunSpec`, `cmd/issue-bot`, `cmd/chat`,
  `cmd/commit-msg`, `cmd/gate`) already build `RequestedConfig` natively — but
  they call `runspec.Resolve` and hand the spec straight to a loop, so they are a
  SECOND unrecorded lane, not a finished migration. All nine producers move.

  ### The defect

  Each of the four flag layers runs `profile.Resolve` ITSELF and hands a
  pre-resolved Config to `Config.Split()`, whose `Requested()` deliberately
  withholds `ExecProfileName`/`TrustProfile` from resolution. Consequences:
  profile threading lives in four surfaces instead of the resolver (correct today
  only because four hand-written call sites happen to agree — the drift class that
  produced the since-fixed headless `AutoVerify` drop), and the emitted
  `ConfigRecord` carries TWO DISAGREEING PROVENANCE VIEWS of the same run: the
  `ResolutionTrace` from the profile-blind `runspec.Resolve` inside `Split()`, and
  `FieldProvenance` from the flag layer's own `profile.Resolve`. The flag layer's
  is the honest one. Until this lands, "one binary" is packaging, not one config
  path.

  ### The seam

  ONE entry point in core, the only way from a request to a run:

      func Prepare(req runspec.RequestedConfig, rt Runtime, content Content,
                   meta RecordInputs) (Prepared, error)

  `req` MUST carry `TrustProfile` + `ExecProfileName` (resolution owns profile
  threading; omitting trust is a hard error, matching the headless refusal).
  `Prepare` calls `runspec.Resolve` exactly once, keeps the Trace, and builds the
  `ConfigRecord` — `RecordMeta` packing stays CENTRAL; surfaces supply only
  surface-specific inputs (invocation surface, binary identity, CLI-override
  list, task hash).

  The invariant is ENFORCED, not asserted: `Prepared`'s fields are unexported
  with read accessors and `Prepare` is its only constructor; the loops become
  `Run(ctx, p Prepared)` / `RunNative(ctx, p Prepared)`, so a bare `ResolvedSpec`
  no longer type-checks at a loop entry. A structural oracle asserts that no
  production package calls a loop entry point except through `Prepared`, and that
  `runspec.Resolve` has no production caller except `Prepare` (test packages
  exempt by import path). Green oracle is a precondition of the deletion.

  ### The gate, split by field class

  S6d changes no EXECUTED behavior, but "byte-identical ConfigRecord" is the
  wrong gate, because unifying resolution necessarily collapses the two
  provenance views:

  | field class | gate |
  |---|---|
  | `ConfigSHA256` (canonical policy value) | byte-identical; any diff is a bug, no allowlist |
  | executed behavior (loops, gates, detectors) | identical; consumption oracle + full suites |
  | `ResolutionTrace` / `ResolutionTraceSHA256` / `FieldProvenance` | expected to change ONCE PER SURFACE, inside that surface's `s` slice, with a reviewed before/after |

  `ResolutionTraceSHA256` is an AUDIT hash, not an identity hash (`ConfigSHA256`
  is identity — that separation is §7.3's purpose), so it is legitimately not
  comparable across the S6d boundary. A schema note lands with the first `s`
  slice so ledger/gobench/bundle readers do not read a trace delta as a policy
  change.

  **One named exception to the `ConfigSHA256` gate, found by the S6d.1 oracle
  (2026-07-13).** `Config.Requested()` withholds `TrustProfile` from resolution,
  so the legacy path ALWAYS resolves the worktree floor as trusted-local's
  (`auto`) — even under `container`/`untrusted`, whose floor demands `required`.
  Today's `ConfigSHA256` therefore MISSTATES the worktree floor on exactly the
  runs where it matters most (eval over public repos runs `container`). It is a
  RECORDING defect, not an execution defect: `PolicyValue.Worktree` has no loop
  consumer — headless forces the worktree from its own profile resolution
  (`plan.ForceWorktree`, `headless/main.go:407`) — so those runs were isolated as
  promised; only the recorded policy lied about why. `Prepare` resolves trust, so
  the floor lands correctly and `ConfigSHA256` MOVES for those two trusts. Pinned
  by `TestPrepareCorrectsWorktreeFloorUnderContainerTrust`. The gate's
  "byte-identical" claim therefore covers the values the legacy path represented
  FAITHFULLY; this is the one it did not.

  ### Presence contract

  Flags are the ONLY policy presence source in all surfaces today (env is read
  solely for secrets; there is no config file; S5 TOML profiles will enter as a
  `RequestedConfig` PRODUCER, not a second presence mechanism). Presence must
  become `fs.Visit`, not value≠zero — today's `optNZ`-based `Requested()` cannot
  distinguish `-stream=false` from unset, so an explicit false silently loses to
  a profile default. That is a REAL behavior change, it affects every surface
  (all four ride the same `Requested()`), and it is therefore NOT bundled into the
  structural work. Each surface migrates as a PAIR:

  - **`s` (structural)** — the native builder reproduces LEGACY presence exactly
    (pointer emitted iff value non-zero), so policy value and behavior are
    provably unchanged.
  - **`p` (presence)** — flip that surface to `Visit` presence; a reviewed
    behavior change, preceded by an `optNZ`-sensitivity AUDIT of that surface
    (bools defaulting true like `-stream`/`-verify-continue`/`-finish-nudge`,
    ints where 0 means disabled, strings where "" means none — only those can
    move).

  Precedence otherwise unchanged: profile declarations < explicit overrides;
  trust-floor tightening always wins; weakening is a startup error. Exactly one
  alias exists (`NavSpiralWindow`), normalized inside `Resolve` before floor
  enforcement, conflicts are errors, builders never pre-normalize.

  ### Slices

  Ordering principle: the core API stays ADDITIVE until every consumer is
  migrated AND deployed. (A green local lab build before a core push proves one
  workspace state; it does not control what another checkout resolves, and the
  wrapper's silent fallback to its last good build is exactly what would mask a
  broken pin — so for the duration of S6d the wrapper warns loudly and exits
  non-zero on rebuild failure rather than serving stale.)

  1. **S6d.1 — `Prepare` seam, additive. SHIPPED 2026-07-13** (`agent/prepare.go`,
     `agent/prepare_test.go`; core + lab suites green). `Prepare(req, rt, content,
     meta RecordInputs) → (Prepared, error)`: resolves once, keeps the trace, and
     derives `RecordMeta` (trust, exec profile + hash, required trust,
     canonicality, per-field provenance) FROM that trace — a caller-supplied
     provenance view is overwritten, pinned by test. `Prepared`'s fields are
     unexported and `Prepare` is its only constructor;
     `RunPrepared`/`RunNativePrepared` are the Prepared-taking loop entries that
     S6d.7 collapses onto `Run`/`RunNative`. Oracles: `Prepare` ≡ `Split` on
     `ConfigSHA256` across trust × CLI (scoped to the trusts the legacy path
     represents faithfully — see the worktree exception above), plus a test
     pinning WHY `Config` is not a legal `Prepare` input (below).

     **Correction to the council's `s`-slice prescription, forced by the code.**
     "Reproduce legacy `optNZ` presence exactly" is UNSOUND for the 19
     profile-covered fields once the profile is forwarded to resolution: an
     explicit `-auto-verify=false` collapses to a zero, `optNZ` reads it as
     unset, and `coding-v2`'s `AutoVerify=true` is resurrected on top of the
     operator's explicit false. (That is exactly why `Requested()` withholds the
     profile — `agent/requested.go:10-13`.) The sound builder uses `fs.Visit`
     presence for the profile-covered fields — which is what every flag layer
     ALREADY does (`headless/flags.go:157` `profileOverrides`), so it is still a
     no-behavior-change migration — and value-presence for the rest. Pinned by
     `TestPreparePresenceContractExplicitFalseSurvivesProfileDefault`, which
     fails if a future builder "simplifies" back to `Config.Requested()` + a
     forwarded profile.
  2. **S6d.2s / .2p — headless.** S6d.2s-a SHIPPED 2026-07-15
     (`headless/configbuild.go` `buildBaseRequest` + `headless/baserequest_test.go`;
     the native request builder + equivalence oracle). S6d.2s-b routes the run
     paths through `Prepare`: the DIRECT single-run path (SHIPPED, live-verified —
     a real run recorded `policy.memory=true` with `field_provenance=profile`)
     and BEST-OF solve+review (SHIPPED — `buildAgentConfigInDir` returns an
     `agent.Prepared`, threading `bestOfCLIOptions.Overrides`). REMAINING: the
     LADDER path, the only cross-repo one — `extras.LadderRequest`'s
     `Base agent.Config` + `Run func(ctx, agent.Config)` in core, consumed by the
     lab's `ladder.Run` which mutates a per-rung `Config`. Migrating it means a
     coordinated core+lab change (base `RequestedConfig`/`Runtime`/`Content` +
     per-rung overrides → one `Prepare` per rung, which is MORE correct — each
     rung gets its own honest record) under the `replace`-pin ordering (lab
     builds green before the core push). Until it lands, a ladder run records the
     old memory/worktree-wrong policy while direct/best-of runs are corrected —
     the transient per-path `ConfigSHA256` inconsistency, scoped to the ladder.

     **A THIRD, BROADER record correction, found by the S6d.2s oracle
     (2026-07-15) — the "ConfigSHA256 byte-identical" gate does NOT hold for
     headless.** `agent.Config` cannot round-trip TWO policy fields through
     `Requested()`, so the legacy `Split` path records a value that disagrees
     with the resolution on ESSENTIALLY EVERY headless run:
     - `memory`: `Config` carries only the `memory.Store` binding, never a policy
       bool, so legacy records `memory=false` even though `coding-v2` and
       `interactive-v2` both declare `Memory=true`.
     - `worktree`: `Requested()` withholds both `TrustProfile` and the exec
       profile, so legacy records the trusted-local floor `auto`, dropping any
       exec-profile demand (`eval-swe-v1` DEMANDS `off`).
     Both are RECORDING-ONLY — no loop/gate/session reads the policy field (the
     store binding and `plan.ForceWorktree` drive behavior) — so `Prepare`
     forwarding the profile corrects the record with zero behavior change, but it
     MOVES `ConfigSHA256` on nearly every headless run. This generalizes S6d.1's
     container worktree-floor finding: the gate is "identical policy EXCEPT the
     documented `correctedFields` {memory, worktree}", asserted field-by-field by
     `TestBaseRequestEquivalentToSplit` + `TestBaseRequestCorrectsDroppedProfile
     Fields`. Prior headless transcripts' recorded `memory`/`worktree` were
     wrong; readers comparing `ConfigSHA256` across the S6d boundary must expect
     these two to move.
     3. **S6d.3s / .3p — TUI** (+ tmux boot drive).
     4. **S6d.4s / .4p — eval** (eval's `p` slice re-runs a pinned reference
     trial and diffs the resolved policy, since a presence flip there can move
     MEASUREMENTS; it may be deferred indefinitely — deletion depends only on the
     `s` slices).
  5. **S6d.4b — the five native producers** (council ×3, issue-bot, chat,
     commit-msg, gate) onto `Prepare`. No `s`/`p` split (they never touched
     Config), but each gains a central `ConfigRecord`, so each gets a reviewed
     record before/after.
  6. **S6d.5 — the oracles.** (a) Per-surface flag→request mapping table: every
     flag set to a distinguishable value, asserting the `RequestedConfig` field
     and its survival into `Prepared`; a flag with no table entry FAILS
     (reflection over the FlagSet). Zero-value expectations are PHASE-SPECIFIC —
     in an `s` slice an explicit zero must map to nil and resolve exactly as
     legacy (the oracle asserts the loss); in the matching `p` slice it must map
     to a non-nil pointer and produce the expected policy change. (b) The
     entry-point enforcement oracle above. (c) A cross-surface contract test: for
     inputs meant to have identical semantics, headless and TUI must resolve to
     the same `ConfigSHA256`, with intended divergences declared as an explicit
     reasoned list — so a NEW divergence fails the build.
     Rationale: the existing oracles cover only the 19 profile FieldIDs; the ~57
     non-profile fields, the Runtime bindings, and Content have no coverage
     against a builder that drops or miswires a flag.
  7. **S6d.6 — test-fixture migration**, performed while BOTH paths exist so each
     migrated file is checked old-vs-new (Resolve-backed fixtures inject
     defaults, validation and floors that direct literals never had — a failure
     here must not be confusable with a deletion failure).
  8. **S6d.7 — deletion.** `agent.Config`, `Split()`, `Requested()`,
     `Bindings()`, `NewSessionFromConfig`, `bridge_s6a_test.go`, the equivalence
     test. Preconditions: the entry-point oracle is green, and the lab's migrated
     commit is the required revision.

  ### Behavior-change budget (exhaustive)

  (a) the trace/provenance unification, once per surface inside its `s` slice — a
  RECORD change, not executed behavior; (b) the three `p` presence slices, each
  preceded by a reviewed audit; (c) nothing else. No flag renames, no new flags
  (the knob freeze holds).

  ### What S6d does NOT buy

  S6d unifies RESOLUTION — one place turns a request into a spec, one place
  records it, no surface resolves profiles privately, and no lane reaches a loop
  unrecorded. That is a PRECONDITION for CLI↔TUI convergence, not convergence.
  It does not equalize per-surface defaults (TUI `reviewed-local`/`interactive-v2`
  with interactive approval vs headless refuse-without-`-trust`/`coding-v2` —
  intended, and they stay), runtime bindings, Content construction,
  session/resume (TUI-only), or approval posture. Those are U2's scope; U2 then
  turns the sanctioned-divergence list from S6d.5(c) into DATA (a named profile
  per surface) instead of code.

  ### What the council killed (v0 → v4)

  - "Headless silently drops AutoVerify" — STALE; S6a fixed it
    (`headless/main.go:209`, `:472`, `:702`). S6d is a pure structural refactor.
  - "Byte-identical ConfigRecord" as the gate — too strong; the trace MUST move
    (two disagreeing provenance views exist today). Gate split by field class.
  - "RecordMeta packing moves into the per-surface builders" — would have
    recreated the exact drift class S6d deletes. It moves into `Prepare`.
  - "Build the lab before every core push" — proves one workspace state; replaced
    by additive-until-deployed ordering + a loud wrapper.
  - A standalone trace-unification slice — fiction; the collapse happens per
    surface, when that surface switches resolvers.
  - "Prepare is the only way to reach a loop" — was unenforced prose while the
    loops still took a bare `ResolvedSpec` and five native producers bypassed it.
    Now structural (unforgeable `Prepared` + enforcement oracle).

Interaction with CONTROL-PLANE Design 1 (loop unification): independent and
prior — a single `ResolvedSpec` shrinks both loops' shared wiring and makes
the eventual unification diff smaller. Do S6 first; do not couple them.

## Open for Robin ⚑

6. ~~Headless profile-field fix — migration shape~~ — RESOLVED 2026-07-11
   (Robin, via "proceed" on the recommendation): take the behavior change
   DIRECTLY — headless runs without `-verify-cmd` gain the soft
   auto-derived verify gate, StandingContext/nudge windows start applying;
   NO `legacy-headless-v1` compat profile (every affected run was silently
   violating its own recorded profile; nothing external depends on the
   buggy posture). Narrowing rationale: council O6 — profiles are global
   contracts, wiring bugs don't get versioned into them.

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
