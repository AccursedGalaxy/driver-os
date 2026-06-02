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
