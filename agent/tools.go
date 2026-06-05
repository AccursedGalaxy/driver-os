package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// DefaultTools is the standard toolbox, every tool acting THROUGH the sandbox
// (P2/P4): file reads and command runs cross the isolation boundary, not the
// bare host.
//
// The Desc is the API the model programs against (P2): it picks and invokes a
// tool from this string alone, never the implementation. Each one states what it
// does, WHEN to use it over the overlapping alternatives, the exact ARG format,
// and what it RETURNS — single-line, because buildSystemPrompt prints one line
// per tool.
// runTimeout is threaded in (not a const) so a caller can lengthen it for a
// longer-running build/test suite; it is interpolated into the `run` Desc below
// so the model is told the REAL limit it is working against (P2 — the Desc is the
// API; a stale "30s" would mislead it once the value is configurable).
func DefaultTools(sb sandbox.Sandbox, runTimeout time.Duration) map[string]Tool {
	return map[string]Tool{
		"list_dir": {
			Name: "list_dir",
			Desc: "list the entries in ONE directory (not recursive). Use this to discover exact paths before read_file — never guess a path. ARG: a directory path relative to the sandbox root (\".\" or \"\" = root). RETURNS: one line per entry as \"dir <name>\" or \"file <name>\", directories first then files, name-sorted.",
			// NativeDesc: behavior + selection only; the `path` schema field owns the format.
			NativeDesc: "List the entries in ONE directory (not recursive). Use it to discover exact paths before reading — never guess a path. Returns one line per entry (\"dir <name>\" / \"file <name>\"), directories first then files, name-sorted.",
			Run:        func(ctx context.Context, arg string) (string, error) { return toolListDir(ctx, sb, arg) },

			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"directory path relative to the sandbox root (\".\" or \"\" = root); not recursive"}},` +
				`"required":["path"]}`),
			RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Path string `json:"path"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", fmt.Errorf("invalid list_dir arguments: %v", err)
				}
				return listDirOp(ctx, sb, a.Path)
			},
		},
		"read_file": {
			Name: "read_file",
			Desc: "read a UTF-8 text file with line numbers. Use AFTER list_dir confirms the path; prefer this over `run cat` for bounded, line-numbered output you can cite. ARG: a path relative to the sandbox root, optionally suffixed with a line range \":<from>-<to>\" (1-based inclusive, e.g. \"main.go:40-80\"; drop <to> as in \"main.go:40-\" to read to end of file) to read part of a large file. RETURNS: the lines, each prefixed \"<n>| \"; long output is clipped with a note telling you the next range to request.",
			// NativeDesc: behavior + selection only; the path/from/to schema fields own the range format.
			NativeDesc: "Read a UTF-8 text file with line numbers, optionally just a line range (from/to). Use after list_dir confirms the path; prefer it over `run cat` for bounded, citable output. Returns the lines prefixed \"<n>| \"; long output is clipped with a note telling you the next range to request.",
			Run:        func(ctx context.Context, arg string) (string, error) { return toolReadFile(ctx, sb, arg) },

			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"file path relative to the sandbox root"},` +
				`"from":{"type":"integer","description":"first line to read (1-based, inclusive); omit to read from the start of the file"},` +
				`"to":{"type":"integer","description":"last line to read (inclusive); omit to read to the end of the file"}},` +
				`"required":["path"]}`),
			RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Path string `json:"path"`
					From *int   `json:"from"`
					To   *int   `json:"to"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", fmt.Errorf("invalid read_file arguments: %v", err)
				}
				// Either bound makes it a ranged read; an absent bound takes its
				// default: from absent => start of file (lo=1), to absent => EOF
				// (hi=0 signals EOF, matching parseReadArg's ":N-"). So {to:50} alone
				// reads the first 50 lines rather than silently dropping `to`.
				lo, hi, hasRange := 1, 0, false
				if a.From != nil {
					lo, hasRange = *a.From, true
				}
				if a.To != nil {
					hi, hasRange = *a.To, true
				}
				return readFileOp(ctx, sb, a.Path, lo, hi, hasRange)
			},
		},
		"run": {
			Name: "run",
			Desc: fmt.Sprintf("run a shell command with `sh -c` inside the sandbox and observe its result. Commands START in the PROJECT ROOT — the SAME root your file-tool paths are relative to — so do NOT `cd` to find the project; just run the command (e.g. `go test ./...`) with paths relative to the root. Use for what the file tools can't do — build, test, grep, git, multi-step pipelines. Prefer list_dir/read_file for plain listing/reading. ARG: one shell command line (pipes, &&, quotes allowed). RETURNS: \"exit <code> (<duration>)\" then stdout/stderr sections; each stream is clipped head+tail if large; the command is killed after %s.", runTimeout),
			// NativeDesc: behavior + selection only; the `command` schema field owns the format.
			NativeDesc: fmt.Sprintf("Run a shell command with `sh -c` inside the sandbox. Commands start in the project root (the same root file-tool paths are relative to), so don't `cd` to locate the project — run the command directly with root-relative paths. Use for what the file tools can't do — build, test, grep, git, multi-step pipelines; prefer list_dir/read_file for plain listing/reading. Returns the exit code and duration, then stdout/stderr (clipped head+tail if large); the command is killed after %s.", runTimeout),
			Run:        func(ctx context.Context, arg string) (string, error) { return toolRun(ctx, sb, arg, runTimeout) },

			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"command":{"type":"string","description":"one shell command line, run with sh -c inside the sandbox (pipes, &&, quotes allowed)"}},` +
				`"required":["command"]}`),
			RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", fmt.Errorf("invalid run arguments: %v", err)
				}
				return runOp(ctx, sb, a.Command, runTimeout)
			},
		},
		"search": {
			Name: "search",
			// Desc is the API the model programs against. It names the ONE thing this
			// tool fixes that `run grep` couldn't: it always searches from the ROOT,
			// so the model cannot accidentally scope away the markdown docs (the
			// retry/backoff "is it already decided?" footgun). The whole arg is the
			// pattern — there is deliberately no folder knob in the text protocol, so
			// the scope can't be narrowed wrong.
			Desc: "search the WHOLE project for a regex PATTERN with ripgrep, always from the sandbox ROOT — so code AND the docs (DESIGN.md, HARD-PROBLEMS.md, README.md, CLAUDE.md) are ALWAYS in scope. Use this over `run grep` to answer \"does X already exist / is X already decided?\": searching the term surfaces the design docs automatically, because you cannot scope a folder away. It respects .gitignore, so noise (.git/, vendored _deps/ clones, eval/runs/ trace dumps) is skipped for you. ARG: the pattern — a Rust regex, e.g. `retry|backoff|Retryable` (the WHOLE arg is the pattern; it is matched from the root). Case-insensitive UNLESS the pattern contains an uppercase letter. RETURNS: matching lines as `path:line:text` (gitignored files excluded); capped, with a note on how to narrow if there are too many.",
			// NativeDesc: behavior + selection only; the `pattern`/`path` schema fields
			// own the format. The native loop DOES expose an optional `path` to narrow,
			// but its default (omitted) is the whole root — so the docs stay in scope
			// unless the model deliberately narrows.
			NativeDesc: "Search the whole project for a regex pattern with ripgrep, from the sandbox root so code AND the docs (DESIGN.md, README.md, …) are always in scope. Prefer it over `run grep` to answer \"does X already exist / is X already decided?\": searching the term surfaces the design docs automatically. Respects .gitignore (skips .git/, vendored clones, trace dumps). Case-insensitive unless the pattern contains an uppercase letter. Returns matching lines as `path:line:text`, capped with a note on how to narrow.",
			Run:        func(ctx context.Context, arg string) (string, error) { return toolSearch(ctx, sb, arg, runTimeout) },

			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"a Rust-regex pattern to search for, e.g. retry|backoff. Case-insensitive unless it contains an uppercase letter."},` +
				`"path":{"type":"string","description":"OPTIONAL directory or file (relative to the sandbox root) to narrow the search to; OMIT to search the whole project — the default, which keeps the docs in scope. Must stay within the root."}},` +
				`"required":["pattern"]}`),
			RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Pattern string `json:"pattern"`
					Path    string `json:"path"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", fmt.Errorf("invalid search arguments: %v", err)
				}
				return searchOp(ctx, sb, a.Pattern, a.Path, runTimeout)
			},
		},
		"write_file": {
			Name: "write_file",
			Desc: "create or OVERWRITE a text file inside the sandbox. PREFER this over `run` with shell redirection (`>`/`tee`): it is confined to the sandbox root and reports exactly what it wrote, where redirection runs unfenced. ARG: the path (relative to the sandbox root), then a space, then the file CONTENT on the SAME line — because the action is one line, write a line break as the two characters \"\\n\" (also \"\\t\" for a tab, \"\\\\\" for a literal backslash). Write the content RAW — do NOT wrap it in surrounding quotes, or the quotes become part of the file. RETURNS: a confirmation with the path, byte count, and line count. NOTE: the parent directory must already exist (make it with `run mkdir -p <dir>` first); writing an existing path REPLACES its contents.",
			// NativeDesc: behavior + selection only. Critically it does NOT mention \n
			// escapes — in native mode `content` is written VERBATIM, so an escape
			// instruction here would make the model write a literal backslash-n.
			NativeDesc: "Create or OVERWRITE a text file inside the sandbox. Prefer this over `run` with shell redirection (`>`/`tee`): it is fence-confined and reports exactly what it wrote. The parent directory must already exist (make it with `run mkdir -p <dir>` first); writing an existing path REPLACES its contents. Returns a confirmation with the path, byte count, and line count.",
			Run:        func(ctx context.Context, arg string) (string, error) { return toolWriteFile(ctx, sb, arg) },

			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"file path relative to the sandbox root; the parent directory must already exist (make it with run mkdir -p first). Writing an existing path REPLACES its contents."},` +
				`"content":{"type":"string","description":"the full file content, written verbatim — real newlines, no escaping, no surrounding quotes"}},` +
				`"required":["path","content"]}`),
			RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", fmt.Errorf("invalid write_file arguments: %v", err)
				}
				return writeFileOp(ctx, sb, a.Path, a.Content)
			},
		},
		"edit_file": {
			Name: "edit_file",
			// edit_file is ANCHOR-BASED, not line-numbered: you give the exact text to
			// change (`old`) and its replacement (`new`). The eval (HP-7) showed
			// line-range editing drifts when a model batches edits without re-reading —
			// a text anchor carries no number to go stale, so it can't drift.
			Desc: "replace the UNIQUE occurrence of a snippet of text in an existing file — surgical and DRIFT-FREE: you give the exact text to change, NOT line numbers, so earlier edits in the same file never invalidate a later one. Use this over write_file for a small change in a large file. Read the file FIRST and copy the OLD text byte-for-byte (indentation included). ARG: `<path> <old text> ||| <new text>` — the literal separator ` ||| ` divides the two; write a line break in either as \"\\n\" (also \"\\t\", \"\\\\\"; do NOT wrap in quotes). The OLD text must match EXACTLY ONE place — if it matches none or several, you'll be told to add surrounding context. Make the NEW text empty (`... |||` with nothing after) to DELETE the old text. RETURNS: a confirmation plus the changed region re-numbered so you can verify it.",
			// NativeDesc: behavior + selection only; the path/old/new schema fields own
			// the format. No \n-escape framing — old/new are written verbatim in native mode.
			NativeDesc: "Replace the UNIQUE occurrence of a text snippet in an existing file — surgical and DRIFT-FREE: you give the exact existing text (`old`) and its replacement (`new`), NOT line numbers, so earlier edits never invalidate a later one. Use it over write_file for a small change in a large file. Read the file FIRST and copy `old` byte-for-byte (indentation included); it must match EXACTLY ONE place — if none or several, you'll be told to add context. Set `new` to \"\" to DELETE the matched text. Returns a confirmation plus the changed region re-numbered so you can verify it.",
			Run:        func(ctx context.Context, arg string) (string, error) { return toolEditFile(ctx, sb, arg) },

			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"file path relative to the sandbox root"},` +
				`"old":{"type":"string","description":"the exact existing text to replace, copied byte-for-byte from read_file output (indentation and whitespace included). Must identify EXACTLY ONE place in the file — include enough surrounding lines to be unique."},` +
				`"new":{"type":"string","description":"the replacement text, written verbatim (real newlines, no escaping). Use an empty string to DELETE the old text."}},` +
				`"required":["path","old","new"]}`),
			RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Path string `json:"path"`
					Old  string `json:"old"`
					New  string `json:"new"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", fmt.Errorf("invalid edit_file arguments: %v", err)
				}
				return editFileOp(ctx, sb, a.Path, a.Old, a.New)
			},
		},
	}
}

// ReadOnlyTools is DefaultTools with the two dedicated MUTATION tools
// (write_file, edit_file) removed: list_dir + read_file + run only. It exists for
// agents whose whole job is to OBSERVE and reason about a codebase without
// changing it — e.g. the issue-review bot, which grounds a discussion in the real
// code and must never modify the repo it analyses.
//
// Honest scope note: this is "no dedicated write tools", not a hermetic read-only
// jail. `run` stays in (grep/build/test is how the agent grounds "does X already
// exist?"), and `run` executes shell, so it CAN touch the filesystem. That is
// acceptable here because the caller pairs it with an ephemeral, confined sandbox
// (the CI checkout) and a trusted-author gate upstream — the bot's task only ever
// comes from accounts you trust. Drop `run` too if you need a stricter set.
//
// It derives from DefaultTools by deletion so the kept tools' descriptions and
// schemas never drift from the canonical set.
func ReadOnlyTools(sb sandbox.Sandbox, runTimeout time.Duration) map[string]Tool {
	t := DefaultTools(sb, runTimeout)
	delete(t, "write_file")
	delete(t, "edit_file")
	return t
}

// ---- tools: real external state (P4) ----

// toolListDir returns NAMES, not a stat dump (P1, "return information not data"):
// the model calls this to learn paths, so kind+name is the minimum it needs. Dirs
// come first then files, each group name-sorted (ListDir is already name-sorted),
// giving the stable shape the model can rely on call-to-call (P6).
func toolListDir(ctx context.Context, sb sandbox.Sandbox, arg string) (string, error) {
	return listDirOp(ctx, sb, arg)
}

// listDirOp is the parse-free core shared by the text handler (toolListDir) and
// the structured native handler (read straight from the `path` field): no arg
// parsing here, just the directory listing and its bounds (P1).
func listDirOp(ctx context.Context, sb sandbox.Sandbox, path string) (string, error) {
	entries, err := sb.ListDir(ctx, path)
	if err != nil {
		return "", explainPathErr(path, "directory", err) // (P3) errors are recovery instructions.
	}
	if len(entries) == 0 {
		return "(empty directory)", nil // stable, unambiguous — not a blank string.
	}
	var dirs, files []string
	for _, e := range entries {
		if e.IsDir {
			dirs = append(dirs, e.Name)
		} else {
			files = append(files, e.Name)
		}
	}
	var b strings.Builder
	shown := 0
	emit := func(kind string, names []string) {
		for _, n := range names {
			if shown >= listEntryCap { // (P1) cap so a 10k-entry dir can't flood the window.
				return
			}
			fmt.Fprintf(&b, "%s %s\n", kind, n)
			shown++
		}
	}
	emit("dir ", dirs)
	emit("file", files)
	if len(entries) > listEntryCap { // (P3) tell the model how to see the rest.
		fmt.Fprintf(&b, "...[%d more entries omitted — narrow with `list_dir <subpath>`]\n", len(entries)-listEntryCap)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// toolReadFile reads a text file with absolute line numbers, obeying BOTH bounds:
// it never pulls more than maxFileBytes off disk (P4, memory — no OOM on a giant
// file) and never returns more than readLineCap lines into context (P1 — no window
// rot), and when it clips it tells the model the exact next range to ask for (P3).
func toolReadFile(ctx context.Context, sb sandbox.Sandbox, arg string) (string, error) {
	path, lo, hi, hasRange, badRange := parseReadArg(arg)
	if badRange != "" { // (P3) teach the fix, don't pretend the file is missing.
		return "", fmt.Errorf("invalid line range %q — use numbers, e.g. \"main.go:40-80\"", badRange)
	}
	return readFileOp(ctx, sb, path, lo, hi, hasRange)
}

// readFileOp is the parse-free core shared by the text handler (after
// parseReadArg) and the structured native handler (after reading typed
// from/to fields). lo/hi are 1-based inclusive; hasRange false reads the whole
// file; hi<=0 with hasRange means "to EOF". It obeys both bounds — never pulls
// more than maxFileBytes off disk (P4) and never returns more than readLineCap
// lines (P1) — and tells the model the next range when it clips (P3).
func readFileOp(ctx context.Context, sb sandbox.Sandbox, path string, lo, hi int, hasRange bool) (string, error) {
	data, overMem, err := readBounded(ctx, sb, path) // (P4) bounded read; never loads the whole 5 GB.
	if err != nil {
		return "", explainPathErr(path, "file", err) // (P3)
	}

	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // a trailing newline yields a phantom empty line; drop it for counts.
	}
	total := len(lines)
	if total == 0 {
		return "(empty file)", nil
	}

	if !hasRange {
		lo, hi = 1, total
	}
	if lo < 1 {
		lo = 1
	}
	if lo > total { // (P3) a recovery-shaped "you overshot" rather than a silent empty read.
		return "", fmt.Errorf("file %q has %d line(s); requested start %d is past the end — read a lower range", path, total, lo)
	}
	if hi <= 0 || hi > total {
		hi = total
	}

	clippedLines := 0
	if hi-lo+1 > readLineCap { // (P1) context bound, distinct from the memory bound above.
		clippedLines = hi - (lo + readLineCap - 1)
		hi = lo + readLineCap - 1
	}

	var b strings.Builder
	width := len(strconv.Itoa(hi)) // right-align numbers so the source column lines up (P1, scannable).
	for n := lo; n <= hi; n++ {
		fmt.Fprintf(&b, "%*d| %s\n", width, n, lines[n-1])
	}
	// Footer: the recovery instructions that make a partial read actionable (P3).
	if clippedLines > 0 {
		fmt.Fprintf(&b, "...[%d more line(s) — read `%s:%d-%d` for the next chunk]\n", clippedLines, path, hi+1, hi+readLineCap)
	}
	if overMem {
		fmt.Fprintf(&b, "...[file exceeds %d KiB; only the first part was read — use line ranges to page through it]\n", maxFileBytes>>10)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// readBounded reads at most maxFileBytes (P4). It prefers the sandbox's optional
// LimitedReader, which never loads more than the cap into memory; if a backend
// lacks it we fall back to a full ReadFile then cap what we KEEP — a degraded
// memory bound (the read itself isn't bounded), documented so it isn't mistaken
// for the real fence. The local backend implements LimitedReader, so today's path
// is the safe one.
func readBounded(ctx context.Context, sb sandbox.Sandbox, path string) (data []byte, overCap bool, err error) {
	if lr, ok := sb.(sandbox.LimitedReader); ok {
		return lr.ReadFileLimit(ctx, path, maxFileBytes)
	}
	data, err = sb.ReadFile(ctx, path)
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > maxFileBytes {
		return data[:maxFileBytes], true, nil
	}
	return data, false, nil
}

// parseReadArg splits "path" or "path:<from>-<to>" (also "path:<from>-" to EOF, or
// "path:N" for a single line). It splits on the LAST colon and only treats the
// suffix as a range if it actually parses as one, so a path containing a colon
// still reads whole.
//
// badRange is the defense-in-depth signal (rarely hit now the desc no longer
// dangles a copyable START/END token): when the colon-suffix LOOKS like a range
// attempt — alnum-dash-alnum, no path characters — but doesn't parse as numbers,
// it's a botched range, not a path-with-colon, so we surface a teaching error
// instead of a misleading "no such file". A genuine filename with a colon (e.g.
// "weird:name.txt") has a dot/slash in the suffix or no dash, so it stays a path.
func parseReadArg(arg string) (path string, lo, hi int, hasRange bool, badRange string) {
	arg = strings.TrimSpace(arg)
	i := strings.LastIndex(arg, ":")
	if i < 0 {
		return arg, 0, 0, false, ""
	}
	spec := arg[i+1:]
	start, end, hadDash := strings.Cut(spec, "-")
	if lo, err := strconv.Atoi(start); err == nil && lo >= 1 {
		switch {
		case !hadDash: // "path:N" — a single line.
			return arg[:i], lo, lo, true, ""
		case end == "": // "path:N-" — to EOF, signalled by hi=0.
			return arg[:i], lo, 0, true, ""
		default:
			if hi, err := strconv.Atoi(end); err == nil {
				return arg[:i], lo, hi, true, ""
			}
		}
	}
	if isRangeShaped(spec) { // looks like a range, isn't a valid one -> botched.
		return arg, 0, 0, false, spec
	}
	return arg, 0, 0, false, "" // a real path that happens to contain a colon.
}

// isRangeShaped reports whether s has the surface form of a line range
// (alnum-dash-alnum, at least one dash, no path/space characters). It deliberately
// matches non-numeric attempts like "START-100" so they're caught as botched
// ranges rather than mistaken for filenames.
func isRangeShaped(s string) bool {
	if !strings.Contains(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r == '-',
			r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}

// dirArg returns the directory a list_dir should target to find path, as a
// sandbox-relative arg. filepath.Dir of a root-level name is ".", which is the
// sandbox root — so a missing top-level file points the model at ".", never "..".
func dirArg(path string) string {
	d := filepath.Dir(strings.TrimSpace(path))
	if d == "." || d == "/" || d == "" {
		return "."
	}
	return d
}

// explainPathErr turns a raw filesystem error into a RECOVERY INSTRUCTION (P3):
// not "open foo: no such file" (which invites a blind retry) but the next move to
// make. Sandbox fence refusals already read well, so they pass through unchanged.
func explainPathErr(path, noun string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Name the CONCRETE directory to list (filepath.Dir), never the relational
		// word "parent" — dogfooding showed the model reads "parent" as a literal
		// "..", which the fence then refuses. And soften the steer: a confirmed
		// absence is a valid conclusion, not an invitation to keep searching (P3).
		return fmt.Errorf("no such %s: %q — run `list_dir %s` to see the exact names; if you've already confirmed that directory's contents, the %s may genuinely not exist and saying so can be your answer", noun, path, dirArg(path), noun)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("permission denied: %q", path)
	}
	switch { // EISDIR / ENOTDIR surface as plain text across backends; match on it.
	case strings.Contains(err.Error(), "is a directory"):
		return fmt.Errorf("%q is a directory, not a file — use `list_dir %s` to see inside it", path, path)
	case strings.Contains(err.Error(), "not a directory"):
		return fmt.Errorf("a path component of %q is a file, not a directory — run `list_dir %s` to find the real path", path, dirArg(path))
	}
	return err
}

// toolWriteFile creates or overwrites a file THROUGH the sandbox fence (P2/P4):
// the write goes through the same resolve() boundary list_dir and read_file obey,
// so a model-authored path can never escape the root — unlike a `run` redirection,
// which executes unfenced on the host. It returns a confirmation (path, bytes,
// lines), NOT an echo of the content (P1, "return information not data"): the model
// just sent those bytes this turn, so repeating them back would only burn context.
// A successful write is real external state, so dispatch will mark the run grounded
// (the write IS an observation of the world changing), the same as any other tool.
func toolWriteFile(ctx context.Context, sb sandbox.Sandbox, arg string) (string, error) {
	path, content, badEscape := parseWriteArg(arg)
	if badEscape != "" { // (P3) teach the escape set rather than write a stray backslash.
		return "", fmt.Errorf("invalid escape %q in content — only \\n, \\t and \\\\ are supported; write a literal backslash as \\\\", badEscape)
	}
	return writeFileOp(ctx, sb, path, content)
}

// writeFileOp is the parse-free core shared by the text handler (after
// parseWriteArg decodes its escapes) and the structured native handler (which
// passes the JSON `content` string verbatim). It holds the bounds and recovery
// messages; the only difference between the two callers is how `content` was
// obtained (escape-decoded vs. a raw JSON string).
func writeFileOp(ctx context.Context, sb sandbox.Sandbox, path, content string) (string, error) {
	if path == "" { // (P3) name the shape, don't just reject.
		return "", fmt.Errorf("write_file needs a path: write the path first, then a space, then the content, e.g. `write_file notes.txt hello\\nworld`")
	}
	if env := looksLikeEditEnvelope(content); env != "" { // (P3/P6) a foreign patch/diff wrapper, not file content.
		return "", fmt.Errorf("content begins with %q, which looks like a patch/diff/fence wrapper rather than the file body — send the RAW file contents only (no apply_patch envelope, no ``` fence, no diff markers)", env)
	}
	if n := len(content); n > writeByteCap { // (P4) backstop; see writeByteCap.
		return "", fmt.Errorf("content is %d bytes, over the %d-byte write_file limit — split it across smaller writes, or generate it with `run`", n, writeByteCap)
	}
	if err := sb.WriteFile(ctx, path, []byte(content), 0o644); err != nil {
		return "", explainWriteErr(path, err) // (P3) recovery instruction, not a raw errno.
	}
	return fmt.Sprintf("wrote %d byte(s), %d line(s) to %s", len(content), lineCount(content), path), nil
}

// parseWriteArg splits "<path> <content...>" into the path (first whitespace-
// delimited token) and the file content (the remainder), decoding the minimal
// escape set the one-line protocol forces (\n, \t, \\). A path containing a space
// is not expressible — rare, and the same simplicity the other tools accept. With
// no content token at all ("write_file empty.txt") it writes a real empty file.
func parseWriteArg(arg string) (path, content, badEscape string) {
	// parseAction already trimmed the outer whitespace; split off the first token.
	i := strings.IndexAny(arg, " \t")
	if i < 0 {
		return arg, "", "" // path only -> truncate-to-empty.
	}
	raw := strings.TrimLeft(arg[i+1:], " \t")
	content, badEscape = unescape(raw)
	return arg[:i], content, badEscape
}

// unescape decodes the escapes a single physical line can carry: "\n" -> newline,
// "\t" -> tab, "\\" -> a literal backslash. A backslash before anything else is a
// botched escape — returned as badEscape so write_file can teach the fix (P3)
// rather than silently writing a stray backslash. This is the honest cost of a
// one-line protocol: a body full of literal backslashes (some regexes, some code)
// is awkward to express, and `run` with a heredoc is the documented escape hatch.
func unescape(s string) (out, badEscape string) {
	if !strings.Contains(s, `\`) {
		return s, "" // fast path: the common case has no escapes.
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			return "", `\` // a trailing lone backslash.
		}
		switch s[i+1] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '\\':
			b.WriteByte('\\')
		default:
			return "", `\` + string(s[i+1])
		}
		i++ // consumed the escaped char too.
	}
	return b.String(), ""
}

// lineCount reports how many lines content occupies on disk: one per newline, plus
// a final line when the content doesn't end in a newline. Empty content is zero
// lines. It feeds only the confirmation string, so the model can sanity-check that
// what landed matches what it meant to write.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// explainWriteErr turns a write failure into a recovery instruction (P3). The
// common case — fs.ErrNotExist — means something DIFFERENT on write than on read:
// not "the file is absent" but "its parent directory is", because os.WriteFile
// does not create parents. So this can't reuse explainPathErr (which would tell the
// model the file may simply not exist — useless when it is trying to create it).
// A fence refusal from resolve() is already a clear instruction and falls through.
func explainWriteErr(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("cannot write %q: its parent directory does not exist — create it first with `run mkdir -p %s`, then write again", path, dirArg(path))
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("permission denied writing %q", path)
	}
	if strings.Contains(err.Error(), "is a directory") {
		return fmt.Errorf("%q is a directory, not a file — choose a file path to write", path)
	}
	return err
}

// replaceSep divides the old text from the new text in edit_file's one-line text
// protocol: `<path> <old> ||| <new>`. It is only used by the text loop — the native
// loop passes typed {path,old,new} fields straight to editFileOp, no parsing.
const replaceSep = " ||| "

// toolEditFile is the text-protocol entry: parse `<path> <old> ||| <new>` (old/new
// carrying write_file's \n,\t,\\ escapes, since one physical line can't hold real
// newlines), then defer to the shared editFileOp core.
func toolEditFile(ctx context.Context, sb sandbox.Sandbox, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	i := strings.IndexAny(arg, " \t")
	if i < 0 { // path only — not enough to act on.
		return "", fmt.Errorf("edit_file needs a path, the exact text to replace, and its replacement: `edit_file main.go OLD%sNEW`", replaceSep)
	}
	path := arg[:i]
	rest := strings.TrimLeft(arg[i+1:], " \t")
	j := strings.Index(rest, replaceSep)
	if j < 0 {
		return "", fmt.Errorf("edit_file needs `%s` between the old text and the new text: `edit_file main.go OLD%sNEW`", strings.TrimSpace(replaceSep), replaceSep)
	}
	oldStr, badEscape := unescape(rest[:j])
	if badEscape != "" {
		return "", fmt.Errorf("invalid escape %q in old text — only \\n, \\t and \\\\ are supported", badEscape)
	}
	newStr, badEscape := unescape(rest[j+len(replaceSep):])
	if badEscape != "" {
		return "", fmt.Errorf("invalid escape %q in new text — only \\n, \\t and \\\\ are supported", badEscape)
	}
	return editFileOp(ctx, sb, path, oldStr, newStr)
}

// editFileOp is the parse-free core shared by the text handler and the structured
// native handler. It replaces the UNIQUE occurrence of old with new — anchored by
// content, not line numbers, so a sequence of edits in one run cannot drift (HP-7):
// the model copies the text to change, never a number a prior edit invalidated. 0 or
// >1 matches are teach-the-fix errors (P3), not guesses; new "" deletes the match.
// The splice leaves every byte outside the match untouched, so the file's
// trailing-newline convention is preserved without special-casing.
func editFileOp(ctx context.Context, sb sandbox.Sandbox, path, oldStr, newStr string) (string, error) {
	if oldStr == "" { // an empty anchor matches everywhere — there's nothing to locate.
		return "", fmt.Errorf("edit_file needs the exact existing text in `old` to locate the edit; to create a new file use write_file")
	}
	if oldStr == newStr {
		return "", fmt.Errorf("`old` and `new` are identical — nothing to change")
	}
	if n := len(newStr); n > writeByteCap {
		return "", fmt.Errorf("replacement is %d bytes, over the %d-byte limit — split the edit, or generate the file with `run`", n, writeByteCap)
	}
	if env := looksLikeEditEnvelope(newStr); env != "" { // (P3/P6) a foreign patch/diff wrapper, not replacement text.
		return "", fmt.Errorf("`new` begins with %q, which looks like a patch/diff/fence wrapper rather than the replacement text — send the RAW replacement only (no apply_patch envelope, no ``` fence, no diff markers)", env)
	}

	data, overMem, err := readBounded(ctx, sb, path)
	if err != nil {
		return "", explainPathErr(path, "file", err) // (P3) missing file -> list_dir.
	}
	if overMem { // (P4) never write back a truncated read.
		return "", fmt.Errorf("file %q exceeds %d KiB — too large to edit_file safely; use `run` with sed/awk for an in-place edit", path, maxFileBytes>>10)
	}

	src := string(data)
	switch n := strings.Count(src, oldStr); {
	case n == 0:
		return "", fmt.Errorf("`old` text not found in %q — it must match the file byte-for-byte (indentation and whitespace included); read_file to copy the exact text", path)
	case n > 1:
		return "", fmt.Errorf("`old` matches %d places in %q — include more surrounding lines in `old` so it identifies exactly one location", n, path)
	}

	body := strings.Replace(src, oldStr, newStr, 1)
	if err := sb.WriteFile(ctx, path, []byte(body), 0o644); err != nil {
		return "", explainWriteErr(path, err)
	}

	// (P4) Re-ground: report the change and echo the changed region with FRESH
	// absolute numbers, so the model verifies it landed where intended.
	startLine := 1 + strings.Count(src[:strings.Index(src, oldStr)], "\n")
	newLineCount := 0
	if newStr != "" {
		newLineCount = strings.Count(newStr, "\n") + 1
	}
	oldLineCount := strings.Count(oldStr, "\n") + 1
	outLines := strings.Split(body, "\n")
	if k := len(outLines); k > 0 && outLines[k-1] == "" {
		outLines = outLines[:k-1] // drop the phantom line a trailing newline yields.
	}
	header := fmt.Sprintf("edited %s: replaced %d line(s) with %d at line %d; file now %d line(s)",
		path, oldLineCount, newLineCount, startLine, len(outLines))
	return header + "\n" + echoRegion(outLines, startLine, newLineCount), nil
}

// echoRegion renders a few lines of the post-edit file around the changed region,
// with absolute line numbers (the read_file format), so the observation re-grounds
// the model (P4) without echoing the whole file (P1). For a delete (count == 0) it
// shows the seam where the lines were removed.
func echoRegion(lines []string, start, count int) string {
	const ctxLines = 2
	from := start - ctxLines
	if from < 1 {
		from = 1
	}
	last := start + count - 1 // inclusive end of the inserted block...
	if count == 0 {
		last = start // ...or the seam, for a delete.
	}
	to := last + ctxLines
	if to > len(lines) {
		to = len(lines)
	}
	if to < from { // nothing left to show (file became empty).
		return "(file is now empty)"
	}
	var b strings.Builder
	width := len(strconv.Itoa(to))
	for n := from; n <= to; n++ {
		fmt.Fprintf(&b, "%*d| %s\n", width, n, lines[n-1])
	}
	return strings.TrimRight(b.String(), "\n")
}

// toolRun is the "write & run hands": it executes a shell command inside the
// sandbox. The fence covers file reads, NOT an arbitrary shell — on the local
// backend (IsolationNone) this runs on the host. That is precisely why the local
// backend is trusted-only; swapping in a container/microVM backend (same
// interface) is what would make `run` safe for untrusted, model-authored code.
func toolRun(ctx context.Context, sb sandbox.Sandbox, arg string, timeout time.Duration) (string, error) {
	return runOp(ctx, sb, arg, timeout)
}

// runOp is the parse-free core shared by the text handler and the structured
// native handler (which reads the `command` field). There is no arg parsing for
// run — the whole arg IS the command line — so the two callers differ only in
// where the string came from.
func runOp(ctx context.Context, sb sandbox.Sandbox, command string, timeout time.Duration) (string, error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Path:    "sh",
		Args:    []string{"-c", command},
		Timeout: timeout, // a runaway command is the sandbox's job to kill (P5); caller-configurable.
	})
	if err != nil {
		return "", err // couldn't start / canceled — dispatch turns it into an observation (P6).
	}
	return formatRun(res), nil
}

// formatRun renders a Result as a compact, scannable observation (HP-10): exit
// code and timing first (P1 — the headline the model needs is "did it work"),
// then stdout/stderr only when non-empty, each clipped head+tail so a 50k-line
// build log can't poison the next N turns.
func formatRun(r *sandbox.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit %d (%s)", r.ExitCode, r.Duration.Round(time.Millisecond))
	if r.TimedOut {
		b.WriteString(" [timed out — narrow the command's work or raise its own limits]")
	}
	b.WriteByte('\n')
	if out := clip(string(r.Stdout), runStreamCap); out != "" {
		fmt.Fprintf(&b, "stdout:\n%s\n", out)
	}
	if errOut := clip(string(r.Stderr), runStreamCap); errOut != "" {
		fmt.Fprintf(&b, "stderr:\n%s\n", errOut)
	}
	if r.ExitCode != 0 && len(r.Stdout) == 0 && len(r.Stderr) == 0 {
		b.WriteString("(no output)\n") // a stable shape even for the silent-failure case (P6).
	}
	return strings.TrimRight(b.String(), "\n")
}

// isRunFailure reports whether a formatRun observation is a non-zero exit. The
// observation always begins "exit <code> (<dur>)", so success is exactly the
// "exit 0 " prefix; anything else with the "exit " prefix is a failure. Used by
// the stagnant-observation detector and the closing verification gate.
func isRunFailure(obs string) bool {
	return strings.HasPrefix(obs, "exit ") && !strings.HasPrefix(obs, "exit 0 ")
}

// isRunSuccess reports whether a formatRun observation is a clean `exit 0` — the
// "build/test green" half of HP-4's near-cap finisher signal. It is deliberately
// NOT the negation of isRunFailure: a `run` the sandbox couldn't even start yields
// an "ERROR: …" observation (P6), which is neither a non-zero exit NOR a success,
// so BOTH predicates return false for it. The finisher must key on a real green
// run, not on the mere absence of a red one.
func isRunSuccess(obs string) bool {
	return strings.HasPrefix(obs, "exit 0 ")
}

// runFingerprint reduces a formatRun observation to the parts that are STABLE
// across identical re-runs, for the stagnant-observation detector's equality
// check. It drops the "(<dur>)" parenthetical from the leading "exit <code> (...)"
// line — wall-clock duration jitters run-to-run and would defeat equality — while
// keeping the exit code and the whole stdout/stderr body (the substantive, stable
// signal of "the same failure again").
func runFingerprint(obs string) string {
	first, rest, found := strings.Cut(obs, "\n")
	if p := strings.IndexByte(first, '('); p >= 0 {
		first = strings.TrimRight(first[:p], " ")
	}
	if found {
		return first + "\n" + rest
	}
	return first
}

// toolSearch is the text-loop entry: the WHOLE arg is the pattern (no folder knob
// in the text protocol, on purpose — that is the footgun this tool removes), so it
// always searches from the root. searchOp does the real work, shared with the
// native handler which can also pass an optional path.
func toolSearch(ctx context.Context, sb sandbox.Sandbox, arg string, timeout time.Duration) (string, error) {
	return searchOp(ctx, sb, arg, "", timeout)
}

// searchOp runs ripgrep from the sandbox ROOT with the right defaults baked in:
// gitignore-respecting (so docs are in, noise is out) and root-scoped (so the
// model can't accidentally exclude the folder that holds the answer — the exact
// grep-scope bug this tool fixes). The model supplies only a pattern (+ optional
// narrowing path); the dangerous knobs are ours, not the model's.
//
// It runs `rg` DIRECTLY (Path:"rg", typed Args) rather than through `sh -c`, so
// the model's pattern can never be shell-interpreted — no quoting, no injection,
// and a pattern that starts with `-` is safe because it is passed via `-e`.
func searchOp(ctx context.Context, sb sandbox.Sandbox, pattern, path string, timeout time.Duration) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" { // (P3) name the shape rather than run an empty search.
		return "", fmt.Errorf("search needs a pattern, e.g. `search retry|backoff` — it is matched as a regex from the project root")
	}
	cleanPath, err := validateSearchPath(path)
	if err != nil {
		return "", err // (P3) the fence message already teaches the fix.
	}

	// Defaults that make the output a clean, citable observation (P1): line numbers
	// for citation, flat `path:line:text` (no heading/color so it parses), and a
	// per-line column cap so one minified/vendored line can't blow up the window.
	args := []string{
		"--line-number",
		"--no-heading",
		"--color=never",
		"--smart-case",
		"--max-columns=250",
		"--max-columns-preview",
		"-e", pattern,
	}
	if cleanPath != "" {
		args = append(args, "--", cleanPath) // `--` so a path can't be read as a flag.
	}

	res, err := sb.Exec(ctx, sandbox.Command{Path: "rg", Args: args, Timeout: timeout})
	if err != nil {
		// rg absent is the one start-failure worth a recovery instruction (P3):
		// fall back to the always-present grep, still from the root.
		if strings.Contains(err.Error(), "executable file not found") {
			return "", fmt.Errorf("ripgrep (rg) is not installed — fall back to `run grep -rn <pattern> .` (search from `.`, the root, so the docs stay in scope)")
		}
		return "", err // couldn't start / canceled — dispatch turns it into an observation (P6).
	}
	if res.TimedOut {
		return "", fmt.Errorf("search timed out after %s — narrow it with a more specific pattern or a path", timeout)
	}
	return formatSearch(res, pattern, cleanPath), nil
}

// validateSearchPath keeps an optional narrowing path inside the root, so the
// "always from the root" guarantee holds even when the model narrows: an absolute
// path or a `..` escape is refused with a teaching message (P3). Empty (the
// default) means "search the whole project" and passes through.
func validateSearchPath(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", nil
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("search path %q must be relative to the sandbox root (omit it to search the whole project)", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refused: search path %q escapes the sandbox root", p)
	}
	return clean, nil
}

// formatSearch renders rg's result as a scannable observation. rg's exit codes
// are a three-way signal, not a pass/fail: 0 = matches, 1 = NO matches (a normal
// outcome, not an error), 2 = a real error (e.g. a bad regex). We translate each
// into the right shape (P3/P6): matches capped with a narrow-hint, a confirmed
// absence phrased as a valid conclusion, and a syntax error surfaced with rg's
// own stderr so the model can fix the pattern.
func formatSearch(r *sandbox.Result, pattern, path string) string {
	scope := "the project"
	if path != "" {
		scope = path
	}
	switch r.ExitCode {
	case 0: // matches.
		lines := strings.Split(strings.TrimRight(string(r.Stdout), "\n"), "\n")
		total := len(lines)
		clipped := 0
		if total > searchMatchCap { // (P1) don't flood the window on a common term.
			clipped = total - searchMatchCap
			lines = lines[:searchMatchCap]
		}
		body := strings.Join(lines, "\n")
		if clipped > 0 { // (P3) tell the model how to see the rest.
			body += fmt.Sprintf("\n...[showing the first %d of %d matches — refine the pattern, or pass a narrower `path`, to see the rest]", searchMatchCap, total)
		}
		return body
	case 1: // no matches — a real answer, not a failure (mirrors read_file's "absence can be your answer").
		return fmt.Sprintf("no matches for /%s/ in %s — the pattern is not present anywhere tracked (gitignored files aside); a confirmed absence can be your answer. Widen the pattern if you expected a hit.", pattern, scope)
	default: // exit 2 (or anything else): a real rg error — surface its stderr so the model can fix it (P3).
		msg := clip(strings.TrimSpace(string(r.Stderr)), runStreamCap)
		if msg == "" {
			msg = fmt.Sprintf("ripgrep exited %d with no message", r.ExitCode)
		}
		return "search error: " + msg
	}
}

// looksLikeEditEnvelope detects a foreign file-EDITING-tool wrapper at the start
// of write/edit content — framing the model pattern-completed into our verbatim
// content field instead of sending the raw file body. DOGFOOD surfaced two of
// these: R3's surrounding quotes and R10's gpt-5-nano leaking the OpenAI
// apply_patch "*** Begin Patch" envelope (written byte-for-byte → a file starting
// with "*** ..." that does not compile). The markers below never legitimately
// begin a source file, so rejecting them with a recovery message (P3/P6) converts
// a silent on-disk corruption into a correctable observation. It returns the
// matched marker for the error message, or "" when the content is clean.
//
// Deliberately conservative to avoid false rejects: the diff headers require a
// trailing space ("--- a/x", not a YAML/front-matter "---\n"), and only a leading
// marker counts (a backtick fence MID-file is fine).
func looksLikeEditEnvelope(content string) string {
	s := strings.TrimLeft(content, " \t\r\n")
	for _, m := range []string{
		"*** Begin Patch", // OpenAI apply_patch envelope (R10 gpt-5-nano).
		"*** Add File:",   // apply_patch per-file directives.
		"*** Update File:",
		"*** Delete File:",
		"```",  // a markdown code fence wrapping the whole body.
		"--- ", // unified-diff old-file header (space distinguishes from YAML "---").
		"+++ ", // unified-diff new-file header.
		"@@ ",  // unified-diff hunk header.
	} {
		if strings.HasPrefix(s, m) {
			return m
		}
	}
	return ""
}
