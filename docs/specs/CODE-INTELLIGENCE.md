# Code intelligence / LSP — direction note

Status: **Tier 1 (diagnostics feed) SHIPPED; session-container + gopls (Tier 2) is
the committed next direction.** The question was
"should we hand the agent Language Server Protocol (LSP) capabilities — gopls for
Go — the way mature harnesses do?" This note records what the research found and
the decided direction, so we don't re-research it. The surprising conclusion:
**for our immediate goal this is mostly an *engineering* feedback-loop problem
(run a build, push the errors), not an LSP-client problem** — the genuinely
LSP-shaped part (semantic navigation) is a real but *secondary* bet. It touches
the research backlog at HP-3 (grounding), HP-5 (verification), HP-7 (granularity).

## The question, and where it came from

The eviction live sweep (`eval/runs/selfhist-eviction-n3/`) left one failure:
glm-5 broke the build with a trivial `loop_tools.go:23: "errors" imported and not
used`, never saw it (the agent's verify gate was off and it never self-checked),
and spun to `hit_cap`. The same class recurs across evals — the selfhist hotspot
`loop_tools.go:96 undefined: sandbox`. **The dominant build-broken failures are
compile/type errors a model that isn't self-checking never learns about.** "Give
the agent an LSP" was the proposed fix. We investigated how high-ranking harnesses
actually do it before committing.

## What the research found (2026-06-03)

Surveyed Cursor, Cline, Roo Code, Continue, Zed, Aider, Codex CLI, Claude Code,
Copilot; the SWE-bench research agents; and the dedicated LSP↔MCP bridges
(Serena, multilspy, mcp-language-server). Key findings:

1. **"Give the agent an LSP" mostly means "give it a diagnostics feed."** The only
   LSP capability that reliably reaches the model across all these tools is
   **diagnostics (errors/warnings)**. Go-to-definition / references / hover /
   rename are almost never exposed natively — when users want them they bolt on an
   external MCP server (Serena). Navigation is the minority path.

2. **The mature *editor-less* pattern is "run the tool," not "run an LSP."** Aider
   and Codex (and Cursor's apply step) get correctness feedback by running a
   **linter/compiler subprocess after each edit** and feeding the result back — not
   by speaking LSP. Aider explicitly evaluated LSP and rejected it: *"more
   cumbersome to deploy for a broad array of languages... users would need to stand
   up an LSP server"* (https://aider.chat/docs/ctags.html). That is exactly our
   situation: an editor-less, sandboxed CLI.

3. **Push beats pull; severity-filter to avoid noise.** Where the model must
   *remember* to call `getDiagnostics` (Claude Code's pull model), it often won't.
   The tools that auto-inject "new problems your edit introduced" (Cline, Roo,
   Aider) get reliable self-correction. Roo's refinement is the answer to the
   multi-file objection (below): **errors auto-pushed, warnings on-demand**
   (https://roocodeinc.github.io/Roo-Code/features/diagnostics-integration).

4. **Editor-mediated diagnostics ship real staleness bugs; running the tool
   yourself dodges them.** LSP diagnostics arrive *asynchronously* after a change,
   and servers like gopls/rust-analyzer lag (gopls is "on save" / cold-index).
   Cline added a configurable 500–5000 ms post-edit settle wait + debounce to stop
   the model chasing phantom errors (https://github.com/cline/cline/issues/4381);
   Claude Code's IDE diagnostics still return stale data
   (https://github.com/anthropics/claude-code/issues/6393). Running `go build`
   yourself returns synchronously — the whole async/staleness class evaporates.

5. **For codebase *understanding* (not correctness), tree-sitter + embeddings won,
   not LSP** (Aider repo-map, Roo/Cursor chunk-embeddings). LSP earns its keep for
   *correctness feedback on edits* and *semantic navigation*, not retrieval.

### The one quantified "LSP helps a lot" result — and why we can't use it

Microsoft's **monitors4codegen** (NeurIPS'23, arXiv 2306.10763): constraining
*decoding* with LSP type-directed completions at `.` points lifts compile rate
**+19–25% with no training**, and makes a 1.1B model beat a 175B one. Striking —
but it is a **decoding-time logit-masking** technique. We call models through
OpenRouter/OpenAI and **do not own the decoder**, so we cannot mask logits. The
proven +20% is structurally unavailable to a provider-API agent. The honest
version for us is the weaker "feed diagnostics/completions back into the loop,"
whose value is shown only *qualitatively* by Cline/Roo/Aider. **Do not justify an
LSP build on that number.**

## gopls specifics (Go)

- `gopls check <file|pkg>` exists and emits positioned `file:line:col: message`
  diagnostics (compile + type + vet) in one shot — **but its CLI is officially
  "experimental… not officially supported"** (https://go.dev/gopls/command-line)
  and pays cold-start indexing on every fresh-process invocation.
- Plain **`go build ./...` + `go vet ./...`** is robust, officially supported,
  zero new dependency, and catches exactly the class biting us (the unused-import
  and `undefined:` errors). It's coarser — package-level, no column positions —
  and that is the only thing given up.
- A **persistent gopls LSP server** amortizes the cold index and gives cheap
  incremental, positioned diagnostics *and* completions — but you own the
  lifecycle/readiness problem, and (see below) our sandbox can't host a long-lived
  process yet.

## Codebase constraints (what's cheap vs expensive here)

- **`Sandbox.Exec` runs to completion — there is no long-lived-process slot.** A
  *persistent* gopls server needs either the **unbuilt `Sessioner` optional
  interface** (stubbed PLANNED in `sandbox/sandbox.go`) or running gopls *outside*
  the sandbox, which breaks isolation for the docker/gVisor backends. A persistent
  LSP client is therefore a real, non-trivial build.
- **A one-shot diagnostics feed needs almost no new machinery.** `agent/loop_tools.go`
  already has the seam: `churnNudge` rides a hint on the observation message when
  the model is stuck (`failRuns`/`edits` cross a threshold), and `lastEditIter` +
  the stagnant detector already track "stuck." Surfacing real `go build` errors
  there is a small, localized change on an existing hook.

## The multi-file objection (why this is *not* a per-edit gate)

A build check *gating* every edit is wrong: a multi-file change is *supposed* to
be red mid-flight (edit file A to call a function not yet written in file B →
`undefined`). And note LSP does **not** escape this — gopls reports that same
`undefined` mid-change, because the backend can't know the change is incomplete;
only the model knows its own plan. So the resolution is about *when/how you
surface*, not *which backend*: surface diagnostics as **information, never a
gate**, and only when the model looks **stuck** (the existing stuck signal), not
after every edit. The *gate* stays at termination (the existing reactive verify
gate). This is the glm-5 case precisely — it never finished, so the finish-gate
never fired; what it needed was *visibility*, not a gate.

## Decision

**Tier 1 (SHIPPED 2026-06-03).** A **stuck-triggered, build-diagnostics feed** via
the existing `Exec` seam, mirroring the churn/finish-nudge pattern. Config
`DiagnoseCmd` (the source command, e.g. `go build ./...`) + `DiagnoseAfterEdits`
(the stuck threshold). When the model has edited `DiagnoseAfterEdits` times WITHOUT
reaching a green build, the loop runs `DiagnoseCmd` and surfaces its errors as an
observation — **information, not a gate** (the gate stays at termination,
`VerifyCmd`). Honors the multi-file objection two ways: the threshold keeps it out
of a legitimately-red mid-flight change, and the counter **resets on any green
`run` or clean build**, so a model that verifies as it goes is never nagged. The
**source/surfacing split** is the point: `diagnoseSource()` is the single seam a
future persistent-gopls client slots behind, leaving the surfacing logic (the
`when`/`how` in both loops) untouched. Wired `-diagnose-cmd`/`-diagnose-after-edits`
in `cmd/agent` and `cmd/eval`; tests in `agent/diagnose_test.go` (fires-when-stuck +
4 negative gates, both loops). NOT yet live-swept — the open validation is re-running
the selfhist-eviction glm-5 failure with the feed on, to confirm it converts the
`hit_cap` into a pass. This is the proactive verifier, reframed correctly.

**Tier 2 (deferred — bigger, the genuinely-LSP-shaped bet).** A **persistent
gopls integration exposing coarse, symbol-level tools** (Serena-style
`find_references`, `definition`, `replace_symbol_body` — symbol identity over line
coordinates: https://github.com/oraios/serena). This is the part that is *actually*
"giving the agent an LSP," and it attacks two documented failures — line-drift
editing (Serena's thesis echoes our anchor-edit fix, HP-7) and surgical-edit
grounding (`undefined: sandbox`, HP-3). It requires building `Sessioner` (or
accepting out-of-sandbox gopls) and the readiness/pre-index lifecycle. **Gate it
on Tier 1 proving the loop benefits and on a measured need — the research shows
most production agents ship without navigation, so do not build it speculatively.**

## Map to the research backlog

- **HP-5 (verification):** the diagnostics feed is the cheap *compile/type* half of
  verification, now with a known-good recipe (run the build, push the errors). The
  *behavioral* half stays hard.
- **HP-3 (grounding):** Tier 2 navigation (hover/def/refs) is the under-explored
  lever for making the model condition on real type info before a surgical edit.
- **HP-7 (action granularity):** Serena's symbol-level editing is a direct,
  externally-validated answer to our own line-drift finding.

Sources captured inline above; fuller survey lives in the session that produced
this note (2026-06-03), with related research notes in the private lab
repository.
