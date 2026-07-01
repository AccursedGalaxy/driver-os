# docs/specs/SKILLS.md — Agent Skills for the driver-os harness

Status: BUILT + LIVE-VALIDATED 2026-06-11 (same day as the plan; slices 1–3
shipped, slice 4 remains conditional on need). Implementation: `agent/skill`
(Load/Discover/Tool), wired in cmd/agent (`-skills`, auto-discovery,
untrusted gate) and cmd/jarvis workers; eval suite `eval/suite/skills`
(`-case skills`); default skills in `.agents/skills/` (humanizer,
driver-os-deps). Validation results in §10. Companion docs: DESIGN.md
(architecture), docs/specs/SANDBOX.md (threat model), HARD-PROBLEMS.md (HP-1 context
policy — skills are partly an answer to it).

## 1. What and why

A **skill** is a folder containing a `SKILL.md` (instructions in Markdown with
YAML frontmatter) plus optional bundled resources (`scripts/`, `references/`,
`assets/`). Skills teach the agent procedural knowledge **only when it needs
it** — progressive disclosure instead of stuffing everything into the system
prompt:

- **Level 1 (always loaded):** each skill's `name` + `description` (~100
  tokens/skill) so the model knows the skill exists and when to reach for it.
- **Level 2 (on demand):** the full SKILL.md body, loaded when the model
  invokes the skill.
- **Level 3 (on demand):** bundled reference files and scripts, read/executed
  by the agent's existing tools. Scripts are *executed, not read* — only
  stdout costs tokens — so bundled context is effectively unbounded.

The runtime contract is tiny: a harness needs **filesystem access** and
**code execution**, both of which we already have (read_file, run).

Evidence it's worth building (SkillsBench, arXiv 2602.12670, Feb 2026):
curated skills gave **+16.2pp average pass rate** across 7 model×harness
configs; harness integration quality mattered (Claude Code +23.3pp while
Codex "frequently neglects provided Skills"). Caveats that shape this plan:
software-engineering tasks gained only +4.5pp (the big wins are
missing-procedural-knowledge domains), 2–3 active skills beat 4+ (+18.6pp vs
+5.9pp), compact/detailed styles beat comprehensive (comprehensive was
**negative**, –2.9pp), and **self-generated skills were net negative**
(–1.3pp) — we curate by hand, we don't auto-generate.

## 2. Format: adopt the open Agent Skills spec verbatim

We implement the open spec (https://agentskills.io/specification — Anthropic's
format, since adopted by Claude Code, OpenAI Codex, Gemini CLI, Copilot,
Cursor, VS Code). Zero invented format means every skill Robin already has in
`~/.claude/skills/`, plus the public collections (anthropics/skills,
google-gemini/gemini-skills), is loadable as-is.

A skill directory:

```
my-skill/
  SKILL.md          # required
  scripts/          # optional, executable helpers
  references/       # optional, docs loaded on demand
  assets/           # optional, templates etc.
```

SKILL.md frontmatter (spec fields; everything else ignored but preserved):

| Field           | Required | Validation                                                                 |
|-----------------|----------|----------------------------------------------------------------------------|
| `name`          | yes      | `^[a-z0-9]+(-[a-z0-9]+)*$`, ≤64 chars, **must equal the directory name**   |
| `description`   | yes      | 1–1024 chars; *what it does AND when to use it* — the sole trigger signal  |
| `license`       | no       | string                                                                     |
| `compatibility` | no       | ≤500 chars; environment requirements                                       |
| `metadata`      | no       | string→string map, client-specific                                         |
| `allowed-tools` | no       | parsed but **ignored in v1** (see §7)                                      |

Parsing rules (Gemini CLI semantics — strict and silent): safe YAML only (no
custom tags), file must start with `---`; a skill with missing/invalid
`name`/`description` or a name/dir mismatch is **skipped with a warning to
stderr**, never a hard error. Body recommendation per spec: <500 lines /
<5k tokens; we additionally **warn at load if the body exceeds 10,000 runes**
because the loop's observation backstop is 12,000 runes (agent/agent.go:89)
and a clipped skill body is worse than a short one.

## 3. Architecture decisions

### D1 — Surfacing: one `skill` meta-tool, not N tools, not prompt injection

Claude Code's design, and the best fit for our loop: a single tool named
`skill` whose **description carries the Level-1 listing** (every skill's
name + description). Why this beats the alternatives:

- One tool in `cfg.Tools` flows through both loops with **zero loop changes**:
  the text loop enumerates `Tool.Desc` into the system prompt
  (agent/agent.go:968 buildSystemPrompt), the native loop advertises
  `NativeDesc` + `Schema` via nativeSchemas (agent/loop_tools.go:701).
- N-tools-per-skill would pollute the tool list and break the text protocol's
  one-line grammar for no benefit; skills are instructions, not behaviors.
- Static prompt injection of all bodies is exactly the context bloat skills
  exist to avoid (and SkillsBench says comprehensive context *hurts*).

Tool shape:

- Text loop: `skill <name>` — Desc is the listing plus the ARG grammar line.
- Native loop: `Schema = {"name": {"type":"string", "enum":[...names]}}` —
  the enum makes invalid names a schema error instead of a wasted turn.
- Invoking returns the rendered SKILL.md body as the observation, prefixed
  with a small header naming the skill and its staged resource dir (§D2).
  Repeat invocation returns `already loaded above` + the header (cheap,
  idempotent, mirrors Claude Code's no-re-read behavior).
- Like memory recall, the body is data, not authority: the header frames it
  as "instructions you asked for; verify file paths with tools" (P4).

### D2 — Skills live on the HOST; resources are staged into the sandbox lazily

Skill folders are host-side configuration, like the system prompt and
personas — the sandbox never needs them until a skill is invoked. Precedent:
`go_doc` (agent/godoc.go) is already a trusted host-side read-only tool.

- The `skill` tool's handler runs **host-side**: it reads SKILL.md from the
  host filesystem. No sandbox round-trip for Level 2.
- **On first invocation**, the handler stages the skill's bundled files into
  the sandbox via `sb.WriteFile` (backend-agnostic — works on local and
  docker without mount surgery) under `<root>/.skills/<name>/`, preserving
  the skill's internal layout. The returned body header states:
  `resources staged at .skills/<name>/ — run scripts and read references from there.`
- `${SKILL_DIR}` occurrences in the body are substituted with
  `.skills/<name>` so spec-conformant skills locate their scripts.
- Skills that were never invoked stage nothing — the workspace stays clean.
- Workspace pollution: v1 accepts `.skills/` appearing in the working tree;
  polish item is appending `.skills/` to `<root>/.git/info/exclude` when the
  fixture is a git repo (NOT .gitignore — never mutate tracked files).
- File sizes: staged files go through WriteFile as-is; cap any single staged
  file at 1 MiB and total at 16 MiB per skill (refuse + warn beyond that).

### D3 — Discovery and precedence

Three sources, later wins on name collision (collision → stderr warning):

1. **User skills dir:** `$XDG_CONFIG_HOME/driver-os/skills/` (matches the
   council corpus convention of `$XDG/driver-os/...`).
2. **Project skills dir:** `<root>/.agents/skills/` — the vendor-neutral
   path Codex and Gemini CLI both scan. We deliberately do NOT scan
   `.claude/skills/` (that's Claude-Code-private; symlink if wanted).
3. **Explicit:** `-skills dir1,dir2` flag — each entry is either a parent
   dir of skill folders or a single skill folder (detected by SKILL.md
   presence). Highest precedence; also the eval/test hook.

Trust gate: when the run is `-untrusted` (workspace contents are
attacker-controlled), **project skills are not loaded** — a cloned repo must
not be able to inject standing instructions into the system prompt. User and
explicit skills still load (the operator chose them). This is the OWASP
AST-01/AST-04 mitigation that costs us nothing.

Auto-discovery is on by default (a present-but-uninvoked skill costs ~100
tokens of listing); `-skills none` disables skills entirely.

### D4 — Context lifecycle: the body is a normal observation

The body enters history as a tool result and lives under the existing
eviction/compaction machinery (generateWithEviction). v1 does **not**
re-attach skill bodies after eviction (Claude Code re-attaches the most
recent 5k tokens per skill within a 25k budget — noted as future work in §9).
Consequence for authors, documented in the listing header: write skills as
**standing instructions**, near-term actionable, not giant manuals.

### D5 — Listing budget

- Per-entry cap: 1,536 chars of description in the listing (Claude Code's
  number); longer descriptions are clipped with `…`.
- Total listing cap: 1% of the model's context window when known, else a
  fixed 8,000-char fallback (Codex uses min(2% ctx, 8k chars)). On overflow:
  **names always stay, descriptions drop** (alphabetical-last first in v1 —
  we have no invocation-frequency data yet), and a warning is logged.
- Practically we expect single-digit skill counts; the budget code is small
  and prevents a silent failure mode later.

### D6 — Explicitly OUT of v1

| Deferred                          | Why                                                            |
|-----------------------------------|----------------------------------------------------------------|
| `allowed-tools` semantics         | It's *pre-approval* not restriction in every major harness; we have no per-tool permission layer to pre-approve against. Parse, ignore, document. |
| `` !`cmd` `` preprocessing        | Shell-at-load is the #1 injection vector (Snyk ToxicSkills: 36.8% of marketplace skills flawed). Skip until a concrete need. |
| Arg substitution (`$ARGUMENTS`)   | Our invoker is the model, not a user typing `/cmd args`.       |
| `model`/`effort` overrides        | No per-turn model switching in the loop today.                 |
| File-watching / live reload       | Runs are short-lived; skills are read at run start.            |
| Marketplace/fetch/install         | Local curated dirs only. Installing == copying a folder.       |
| Skill auto-generation             | SkillsBench: net negative. Hand-curate.                        |

## 4. Package design: `agent/skill`

New package `github.com/AccursedGalaxy/driver-os/agent/skill` (subpackage so
`agent` doesn't grow; depends on `agent` only for the `Tool` type — if that
import cycle bites, the constructor moves to `agent/skills.go` instead and
the package keeps only parse/discover).

```go
package skill

// Skill is one parsed, validated skill folder.
type Skill struct {
    Name        string            // == dir base name, validated
    Description string            // 1–1024 chars
    Dir         string            // absolute host path of the skill folder
    Body        string            // SKILL.md content below the frontmatter
    Meta        map[string]string // passthrough: license, compatibility, metadata.*
    Resources   []string          // relative paths of bundled files (scripts/, references/, assets/, …)
}

// Load parses one skill folder. Returns (nil, reason) for spec-invalid
// folders — callers warn and skip, never fail the run.
func Load(dir string) (*Skill, error)

// Discover walks the three source layers in precedence order and returns
// the merged, deduped, name-sorted set plus warnings (collisions, invalid
// folders, oversized bodies).
type Sources struct {
    UserDir    string   // "" => skip
    ProjectDir string   // "" => skip (set "" when untrusted)
    Explicit   []string // -skills entries
}
func Discover(s Sources) (skills []*Skill, warnings []string)

// Tool builds the single `skill` meta-tool over the set. The handler reads
// host-side, stages resources into sb on first invocation, substitutes
// ${SKILL_DIR}, and serves repeat invocations cheaply.
func Tool(skills []*Skill, sb sandbox.Sandbox, ctxWindow int) agent.Tool
```

Internal details:

- Frontmatter parsing with `gopkg.in/yaml.v3` (already in the module graph
  via deps — verify; else add) into a strict struct; `yaml.Node` custom tags
  rejected by using plain `Unmarshal` on a typed struct.
- `Tool` keeps a `map[string]bool` staged-set guarded by a mutex (the native
  loop can dispatch parallel tool calls).
- Listing builder implements D5 budgets; takes `ctxWindow` (0 => 8k-char
  fallback) so eval can exercise the overflow path deterministically.
- Empty skill set => `Tool` is not constructed at all (callers check
  `len(skills) == 0`), so skill-free runs have a byte-identical system
  prompt to today — important for eval comparability.

## 5. Wiring (call sites)

The loop needs **zero changes** (this was the key finding of the codebase
mapping: tools are data; buildSystemPrompt and nativeSchemas already render
whatever is in `cfg.Tools`). All wiring is at the orchestration layer:

1. **cmd/agent/main.go** — the reference wiring:
   ```go
   skillsFlag := flag.String("skills", "", "extra skill dirs (comma-sep); 'none' disables skills")
   // after sandbox construction:
   skills, warns := skill.Discover(skill.Sources{
       UserDir:    userSkillsDir(),                  // $XDG_CONFIG_HOME/driver-os/skills
       ProjectDir: projectSkillsDir(cwd, *untrusted), // <cwd>/.agents/skills, "" if untrusted
       Explicit:   splitSkills(*skillsFlag),
   })
   // log warns; then:
   tools := agent.DefaultTools(sb, runTimeout)
   if len(skills) > 0 {
       tools["skill"] = skill.Tool(skills, sb, ctxWindowFor(model))
   }
   cfg.Tools = tools
   ```
2. **eval** — `Case.Tools` already exists as the override hook
   (eval/eval.go); skill cases build the tool there. No eval-core changes.
3. **jarvis / duet / colony** — same three lines each, when wanted; nothing
   breaks if they don't wire it (Tools=nil keeps DefaultTools). Defer until
   a concrete use (jarvis workers are the likely first customer).

## 6. Eval plan (validates the feature, not just the code)

SkillsBench's sharpest lesson: skills shine where the model lacks procedural
knowledge, not on generic SWE tasks. Design cases accordingly, all in
`eval/suite/skills/`:

- **S1 — procedural-gap A/B (the headline case).** Task that requires
  project-idiosyncratic procedure the model cannot guess — e.g. "produce a
  release note using this repo's exact internal format and validation
  script", with the format/script existing only inside a `release-notes`
  skill (references + a checker script). Arms: baseline (no skills) vs
  skills-on. Oracle: output passes the bundled checker. Expectation:
  near-0% baseline, high pass with skill. This proves Level 2 AND Level 3
  (script execution from `.skills/`).
- **S2 — trigger precision.** Skills-on with 3 loaded skills, task matches
  exactly one. Oracle greps the transcript: the right skill invoked, the
  irrelevant ones not. Catches description-quality and over-triggering.
- **S3 — negative control (do no harm).** A task from the existing dogfood
  corpus that needs no skill, run with 3 irrelevant skills loaded vs
  baseline. Oracle: pass-rate and token deltas ≈ 0. SkillsBench saw
  per-task regressions up to –39pp; this is the guardrail.
- **S4 — corpus regression.** `make corpus-regress` with skills wired but
  empty set: byte-identical prompts (per §4), so the suite must be
  unchanged. Cheap CI-level invariant.

Models: the standard ladder (gemini-3-flash cheap baseline, deepseek-v4-flash
value, gpt-5.5 flagship) per the model-selection policy. Both protocols
(text + native) for S1, since the listing renders differently in each.

## 7. Security posture

Threat model deltas (see docs/specs/SANDBOX.md for the base):

- A skill body is **standing instructions injected into the agent's
  context** — equivalent in power to the persona. Therefore: source trust
  is the control. User + explicit dirs are operator-chosen (trusted);
  project dirs are repo-controlled (excluded under `-untrusted`, §D3).
- Bundled scripts execute **inside the sandbox** via the normal `run` tool —
  they get exactly the isolation the run already has (P2: the sandbox is
  the boundary). Host-side code never executes skill content; it only reads
  and copies it. The `skill` tool itself runs no shell.
- Safe YAML, validated names (no traversal — `name` regex forbids `/` and
  `..` by construction; staged paths are cleaned and verified to stay under
  `.skills/<name>/`), staged-size caps (§D2).
- No `` !`cmd` ``, no network fetch, no auto-install (§D6).

## 8. Build order (tracer-bullet slices)

**Slice 1 — core (one PR):** `agent/skill` package: Load/Discover/Tool with
frontmatter validation, listing budget, lazy staging, ${SKILL_DIR}
substitution; unit tests (table-driven: valid/invalid frontmatter, name-dir
mismatch, collision precedence, listing overflow, staging path traversal
attempt, idempotent re-invoke); wire cmd/agent `-skills` + auto-discovery +
untrusted gate. Verify: hand-run against a toy skill in both protocols on
gemini-3-flash; confirm the body lands as an observation and a bundled
script runs from `.skills/`.

**Slice 2 — eval validation:** S1–S4 cases + one real skill worth keeping
(candidate: a `driver-os-deps` skill encoding CLAUDE.md's
read-deps-from-module-cache procedure — it's exactly the "project-specific
procedural knowledge" skills are for, and we can dogfood it on our own
tasks). Run the A/B, record under eval/runs/, write findings into this doc.

**Slice 3 — fleet wiring + polish:** jarvis worker wiring (workers inherit
the orchestrator's skill set, or per-project skills from the JARVIS-PROJECTS
registry); `.git/info/exclude` staging polish; collision/warning UX;
`-skills none`.

**Slice 4 (only if eval says skills earn their keep):** post-eviction
re-attachment of invoked skill bodies (Claude Code's 5k/25k policy);
`allowed-tools` once a permission layer exists; invocation-frequency-aware
listing degradation.

## 9. Open questions

- **Eviction interplay (HP-1):** if a skill body is evicted mid-run and the
  model re-invokes, v1 serves `already loaded above` — wrong if the load was
  evicted. Fix candidates: track eviction and allow re-serve, or slice-4
  re-attachment. Needs a real observed failure before engineering it.
- **Text-loop listing length:** with many skills the `skill` tool's Desc
  makes the text system prompt long; native mode is unaffected (tool
  descriptions live in the schema). If text-mode evals regress, move the
  listing to a separate system-prompt section for text only.
- **Per-skill memory:** should mneme consolidate "skill X helped on task
  shape Y" facts to bias future triggering? Out of scope; revisit after S2
  data exists.

## 10. Build + validation record (2026-06-11)

What shipped, and where it deviates from the plan above:

- **`agent/skill`** — Load (safe-YAML frontmatter, spec validation, resource
  walk with symlink-skip + size caps), Discover (3 layers, later-wins,
  warnings not errors), Tool (single `skill` meta-tool: single-line Desc for
  the text protocol, line-per-skill NativeDesc, name-enum Schema, lazy
  staging via mkdir-Exec + WriteFile, exec bits preserved, ${SKILL_DIR} AND
  ${CLAUDE_SKILL_DIR} substituted, idempotent re-invoke, mutex'd for parallel
  native calls). Unit-tested (parse/validate/precedence/listing-budget/
  staging/exclude).
- **Deviations from the plan:** an over-1024-char description is clipped with
  a warning rather than skipped (more useful); listing overflow degrades to
  name-only entries (no placeholder text); the `.git/info/exclude` polish
  shipped in slice 1, not 3; jarvis worker wiring shipped (slice 3) —
  workers inherit user skills + the exported project tree's `.agents/skills`,
  no untrusted gate because a registered project is trusted delegation;
  ctxWindow is currently always 0 (the provider doesn't expose the window),
  so the listing budget is the fixed 8,000-char fallback.
- **Default skills:** `.agents/skills/humanizer` (the 34 KB upstream SKILL.md
  was over the observation backstop, so it was restructured the way the spec
  itself prescribes: a ~5.4 KB core procedure + the full pattern guide as
  `references/full-guide.md`, MIT attribution kept) and
  `.agents/skills/driver-os-deps` (CLAUDE.md's module-cache procedure).

Live validation (all on cheap models, both protocols):

- **S1 A/B (gemini-3-flash, n=2 each, eval/runs/20260611T204007Z-d289cb7/):**
  baseline 0/2 (both hit_cap, ~109K prompt tokens, $0.0345/trial burned
  guessing the unguessable format) vs skilled **2/2** (6 iters, ~11K tokens,
  $0.0037/trial). The skill arm is both the only one that passes AND ~9×
  cheaper — progressive disclosure paying for itself.
- **S2 trigger precision: 2/2** — with 3 skills loaded the model loaded
  exactly `release-notes`, never the distractors. **S3 negative control:
  2/2** — correct answers, zero stray skill loads, ~5K tokens.
- **Manual runs:** native + text protocol both load → stage → execute the
  bundled checker from `.skills/<name>/`; humanizer dogfood (auto-discovered
  from `.agents/skills`) triggered organically on turn 2 and produced a
  rewrite with zero em dashes/AI-vocab/emoji; driver-os-deps on
  deepseek-v4-flash followed the skill's procedure (go.mod → go_doc → module
  cache source) to the exact pinned answer; `-skills none` suppresses
  discovery; a skill-free run leaves Config.Tools nil (byte-identical prompt).

Known issues / follow-ups:

- Pre-existing (NOT from this work): `eval` TestGitFixtureMaterialize fails
  on master since aa367ce moved the mneme replace from go.mod to go.work —
  the selfhist fixture rewrite no longer fires (and a materialized fixture
  may not build against the workspace-pinned mneme). Needs its own fix.
- Slice 4 unchanged: post-eviction re-attachment, allowed-tools enforcement,
  invocation-frequency listing degradation — wait for an observed need.
- Wire a real ctxWindow into skill.Tool when providers expose their window.

## Sources

Open spec: https://agentskills.io/specification · Claude Code skills:
https://code.claude.com/docs/en/skills · Best practices:
https://platform.claude.com/docs/en/agents-and-tools/agent-skills/best-practices ·
Engineering: https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills ·
Codex: https://developers.openai.com/codex/skills · Gemini CLI:
https://geminicli.com/docs/cli/skills/ · SkillsBench:
https://arxiv.org/html/2602.12670v1 · Snyk ToxicSkills:
https://snyk.io/blog/toxicskills-malicious-ai-agent-skills-clawhub/ · OWASP
Agentic Skills Top 10: https://owasp.org/www-project-agentic-skills-top-10/ ·
Reference skills: https://github.com/anthropics/skills
