---
name: driver-os-deps
description: |
  Read Go dependency source and API docs the right way in this repo. Use
  whenever you need to know what a dependency's API looks like, how to call a
  library function, what fields a type has, or how a dep behaves — BEFORE
  guessing an API from memory or reaching for web search. Covers the go_doc
  tool, the module cache, and grepping dependency source at the exact pinned
  version.
license: MIT
---

# Reading dependency source in driver-os

Dependencies are Go modules: their full source is ALREADY ON DISK at the exact
pinned version — the same bytes the compiler sees. Prefer that over web search
or remembered API shapes, which drift from the version this project imports.

## Procedure

1. **API question** ("what are the params of X? what fields does Y have?"):
   use the `go_doc` tool first.
   - Overview: `go_doc` with package `github.com/openai/openai-go`
   - A symbol: package + symbol `ChatCompletionNewParams`
   - The whole API: set `all`
   - Actual implementation: set `src`
   It prints a `source: <dir>` line — an absolute path you can `read_file` and
   `search` directly (read-only), on every sandbox backend.

2. **Behavior question** ("how does this lib actually handle retries?"): read
   the source. Find the dir from go_doc's `source:` line, then `read_file`
   the relevant file, or `search` with that absolute dir as the path to grep
   the whole dependency.

3. **Usage-example question** ("how do I call this correctly?"): the
   `*_test.go` files inside the module are working call patterns at the exact
   version. Many libs also ship an `api.md`. List the source dir and read
   them.

4. The pinned version is in `go.mod` / `go.sum` (read_file them). The module
   cache is read-only and cannot report a method that doesn't exist in our
   version — trust it over any web result.

## Rules

- Never answer an API question from memory when go_doc can answer it; a wrong
  guessed signature costs a failed build and a wasted turn.
- Only consider web search if the SOURCE genuinely cannot answer (rare:
  e.g. release timelines, maintainer intent).
- For non-Go deps or git history/issues, the gitignored `_deps/` dir holds
  cloned repos (see `_deps/README.md`); check whether the clone exists before
  assuming.
