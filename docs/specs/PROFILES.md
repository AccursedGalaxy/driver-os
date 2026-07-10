# PROFILES — unified trust + execution profiles

Status: SPEC, council-grilled 2026-07-10 (grill-with-council, critic
`openai/gpt-5.6-sol`, consult run `20260710-101650-c634ff`, 6 rounds; builds
on decision run `20260710-094837-ce9409` = docs/specs/REVIEW-TRIAGE-2026-07-10.md
R1+R2). Not yet implemented. Open items needing Robin are marked ⚑.

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
| `reviewed-local` | gated(local) | none | REQUIRED | REQUIRED (see §1.2) | clean-env allowlist | unrestricted |
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

- **S1 — recording** (zero behavior change): versioned canonical
  serialization of TODAY'S effective config + hashes (prompt, tool-schema,
  config), binary/invocation identity, dirty-build marker. Profile fields
  reserved/nullable. Ships alone; immediate reproducibility win.
- **S2 — trust layer** (two sub-slices, atomic activation):
  S2a lands the resolver package, descriptor table, predicates, conformance
  tests, gated reviewed-local mechanics — inert. S2b atomically flips
  fail-closed headless + updates every in-repo call site (delegate.sh →
  `-trust trusted-local`; eval-on-public-repos → `container`) in one
  commit. No bypassable intermediate release.
- **S3 — execution profiles**: built-ins, non-safety precedence (CLI >
  profile > default), full override provenance, TUI/headless defaults
  become named profiles.
- **S4 — routing-policy artifact + campaign manifests** (spec may proceed in
  parallel earlier; binds to S3 identities).
- **S5 — TOML user profiles; subcommand CLI rides the separate unification
  decision.**

## Open for Robin ⚑

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
