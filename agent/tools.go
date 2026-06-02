package agent

import (
	"context"
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
func DefaultTools(sb sandbox.Sandbox) map[string]Tool {
	return map[string]Tool{
		"list_dir": {"list_dir",
			"list the entries in ONE directory (not recursive). Use this to discover exact paths before read_file — never guess a path. ARG: a directory path relative to the sandbox root (\".\" or \"\" = root). RETURNS: one line per entry as \"dir <name>\" or \"file <name>\", directories first then files, name-sorted.",
			func(ctx context.Context, arg string) (string, error) { return toolListDir(ctx, sb, arg) }},
		"read_file": {"read_file",
			"read a UTF-8 text file with line numbers. Use AFTER list_dir confirms the path; prefer this over `run cat` for bounded, line-numbered output you can cite. ARG: a path relative to the sandbox root, optionally suffixed with a line range \":<from>-<to>\" (1-based inclusive, e.g. \"main.go:40-80\"; drop <to> as in \"main.go:40-\" to read to end of file) to read part of a large file. RETURNS: the lines, each prefixed \"<n>| \"; long output is clipped with a note telling you the next range to request.",
			func(ctx context.Context, arg string) (string, error) { return toolReadFile(ctx, sb, arg) }},
		"run": {"run",
			"run a shell command with `sh -c` inside the sandbox and observe its result. Use for what the file tools can't do — build, test, grep, git, multi-step pipelines. Prefer list_dir/read_file for plain listing/reading. ARG: one shell command line (pipes, &&, quotes allowed). RETURNS: \"exit <code> (<duration>)\" then stdout/stderr sections; each stream is clipped head+tail if large; the command is killed after 30s.",
			func(ctx context.Context, arg string) (string, error) { return toolRun(ctx, sb, arg) }},
		"write_file": {"write_file",
			"create or OVERWRITE a text file inside the sandbox. PREFER this over `run` with shell redirection (`>`/`tee`): it is confined to the sandbox root and reports exactly what it wrote, where redirection runs unfenced. ARG: the path (relative to the sandbox root), then a space, then the file CONTENT on the SAME line — because the action is one line, write a line break as the two characters \"\\n\" (also \"\\t\" for a tab, \"\\\\\" for a literal backslash). Write the content RAW — do NOT wrap it in surrounding quotes, or the quotes become part of the file. RETURNS: a confirmation with the path, byte count, and line count. NOTE: the parent directory must already exist (make it with `run mkdir -p <dir>` first); writing an existing path REPLACES its contents.",
			func(ctx context.Context, arg string) (string, error) { return toolWriteFile(ctx, sb, arg) }},
	}
}

// ---- tools: real external state (P4) ----

// toolListDir returns NAMES, not a stat dump (P1, "return information not data"):
// the model calls this to learn paths, so kind+name is the minimum it needs. Dirs
// come first then files, each group name-sorted (ListDir is already name-sorted),
// giving the stable shape the model can rely on call-to-call (P6).
func toolListDir(ctx context.Context, sb sandbox.Sandbox, arg string) (string, error) {
	entries, err := sb.ListDir(ctx, arg)
	if err != nil {
		return "", explainPathErr(arg, "directory", err) // (P3) errors are recovery instructions.
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
	if path == "" { // (P3) name the shape, don't just reject.
		return "", fmt.Errorf("write_file needs a path: write the path first, then a space, then the content, e.g. `write_file notes.txt hello\\nworld`")
	}
	if badEscape != "" { // (P3) teach the escape set rather than write a stray backslash.
		return "", fmt.Errorf("invalid escape %q in content — only \\n, \\t and \\\\ are supported; write a literal backslash as \\\\", badEscape)
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

// toolRun is the "write & run hands": it executes a shell command inside the
// sandbox. The fence covers file reads, NOT an arbitrary shell — on the local
// backend (IsolationNone) this runs on the host. That is precisely why the local
// backend is trusted-only; swapping in a container/microVM backend (same
// interface) is what would make `run` safe for untrusted, model-authored code.
func toolRun(ctx context.Context, sb sandbox.Sandbox, arg string) (string, error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Path:    "sh",
		Args:    []string{"-c", arg},
		Timeout: 30 * time.Second, // a runaway command is the sandbox's job to kill (P5).
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
