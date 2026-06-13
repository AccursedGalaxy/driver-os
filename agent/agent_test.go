package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

func TestRunTimeoutIsConfigurable(t *testing.T) {
	sb := sbWith(t, nil)
	tools := DefaultTools(sb, 100*time.Millisecond)
	// The Desc must state the REAL timeout, not a stale "30s" (P2 — the Desc is the API).
	if !strings.Contains(tools["run"].Desc, "100ms") {
		t.Errorf("run Desc does not reflect the configured timeout:\n%s", tools["run"].Desc)
	}
	// `sleep 5` under a 100ms cap is killed at ~100ms (the test does not wait 5s).
	out, err := tools["run"].Run(context.Background(), "sleep 5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("run output = %q, want a 'timed out' marker", out)
	}
}

// TestRunDescStatesWorkingDir guards the cwd invariant the run tool MUST teach:
// `run` commands start in the project root (the same root file tools are relative
// to), so the model must not `cd` to "find" the project. Backends disagree on the
// ABSOLUTE cwd (local = host path, docker = /workspace), so the Desc states the
// portable invariant, not a path. Regression guard for the dogfood finding where a
// model assumed a chroot at `/`, ran `cd / && go test`, and burned turns.
func TestRunDescStatesWorkingDir(t *testing.T) {
	tools := DefaultTools(sbWith(t, nil), time.Second)
	for _, field := range []struct {
		name string
		desc string
	}{
		{"Desc", tools["run"].Desc},
		{"NativeDesc", tools["run"].NativeDesc},
	} {
		d := strings.ToLower(field.desc)
		if !strings.Contains(d, "project root") {
			t.Errorf("run %s does not state commands start in the project root:\n%s", field.name, field.desc)
		}
		if !strings.Contains(d, "cd") {
			t.Errorf("run %s does not warn against cd-ing to find the project:\n%s", field.name, field.desc)
		}
	}
}

// sbWith builds a local sandbox over a temp dir seeded with the given files.
func sbWith(t *testing.T, files map[string]string) sandbox.Sandbox {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestParseReadArg(t *testing.T) {
	cases := []struct {
		in       string
		path     string
		lo, hi   int
		hasRange bool
		bad      string
	}{
		{"main.go", "main.go", 0, 0, false, ""},
		{"main.go:40-80", "main.go", 40, 80, true, ""},
		{"main.go:40-", "main.go", 40, 0, true, ""},                            // to EOF
		{"main.go:40", "main.go", 40, 40, true, ""},                            // single line
		{"weird:name.txt", "weird:name.txt", 0, 0, false, ""},                  // colon + dot, no dash -> path
		{"a:b:10-20", "a:b", 10, 20, true, ""},                                 // split on LAST colon
		{"long.txt:START-100", "long.txt:START-100", 0, 0, false, "START-100"}, // botched range (the S3 bug)
		{"long.txt:START-1", "long.txt:START-1", 0, 0, false, "START-1"},       // botched range
		{"main.go:0-5", "main.go:0-5", 0, 0, false, "0-5"},                     // range-shaped but start < 1
		{"file:2024-q1", "file:2024-q1", 0, 0, false, "2024-q1"},               // accepted false-positive
	}
	for _, c := range cases {
		p, lo, hi, hr, bad := parseReadArg(c.in)
		if p != c.path || lo != c.lo || hi != c.hi || hr != c.hasRange || bad != c.bad {
			t.Errorf("parseReadArg(%q) = (%q,%d,%d,%v,%q), want (%q,%d,%d,%v,%q)",
				c.in, p, lo, hi, hr, bad, c.path, c.lo, c.hi, c.hasRange, c.bad)
		}
	}
}

func TestReadFileBotchedRangeIsRecovery(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.txt": "a\nb\n"})
	_, err := toolReadFile(context.Background(), sb, "f.txt:START-100")
	if err == nil || !strings.Contains(err.Error(), "invalid line range") {
		t.Errorf("botched range err = %v, want an 'invalid line range' recovery message", err)
	}
}

func TestReadFileLineNumbersAndRange(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.txt": "a\nb\nc\nd\ne\n"})
	ctx := context.Background()

	whole, err := toolReadFile(ctx, sb, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := "1| a\n2| b\n3| c\n4| d\n5| e"; whole != want {
		t.Errorf("whole read =\n%q\nwant\n%q", whole, want)
	}

	// A range shows ABSOLUTE line numbers, not 1-based-within-range.
	ranged, err := toolReadFile(ctx, sb, "f.txt:3-4")
	if err != nil {
		t.Fatal(err)
	}
	if want := "3| c\n4| d"; ranged != want {
		t.Errorf("ranged read = %q, want %q", ranged, want)
	}
}

func TestReadFileOvershootIsRecoverable(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.txt": "only\ntwo\n"})
	_, err := toolReadFile(context.Background(), sb, "f.txt:99-100")
	if err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Errorf("overshoot err = %v, want a 'past the end' recovery message", err)
	}
}

func TestReadFileLineCap(t *testing.T) {
	var sbBody strings.Builder
	for i := 0; i < readLineCap+50; i++ {
		sbBody.WriteString("x\n")
	}
	sb := sbWith(t, map[string]string{"big.txt": sbBody.String()})
	out, err := toolReadFile(context.Background(), sb, "big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "|"); n > readLineCap+1 { // +1 tolerance for the footer
		t.Errorf("returned %d numbered lines, want <= %d", n, readLineCap)
	}
	if !strings.Contains(out, "more line(s)") {
		t.Errorf("clipped read lacks a 'next chunk' recovery footer:\n%s", out[len(out)-120:])
	}
}

func TestReadFileNotFoundIsRecovery(t *testing.T) {
	sb := sbWith(t, nil)
	_, err := toolReadFile(context.Background(), sb, "nope.txt")
	if err == nil || !strings.Contains(err.Error(), "list_dir") {
		t.Errorf("not-found err = %v, want a message pointing at list_dir", err)
	}
}

func TestListDirDirsFirst(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"zebra.txt":   "z",
		"apple.txt":   "a",
		"sub/keep.go": "k",
	})
	out, err := toolListDir(context.Background(), sb, ".")
	if err != nil {
		t.Fatal(err)
	}
	want := "dir  sub\nfile apple.txt\nfile zebra.txt"
	if out != want {
		t.Errorf("list_dir =\n%q\nwant dirs-first, name-sorted\n%q", out, want)
	}
}

func TestListDirEmpty(t *testing.T) {
	sb := sbWith(t, nil)
	out, _ := toolListDir(context.Background(), sb, ".")
	if out != "(empty directory)" {
		t.Errorf("empty list = %q, want a stable sentinel", out)
	}
}

// When the harness's own run-transcript files leak into the sandbox workspace
// (a caller pointed TranscriptDir at the CWD), list_dir must hide them so the
// agent can't list, read, and derail on its own trace. Real project files with
// names that merely RESEMBLE the pattern stay visible.
func TestListDirExcludesTranscriptArtifacts(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"main.go":                          "package main\n",
		"runs.jsonl":                       "{}\n",        // the longitudinal index
		"20260605-143022-a1b2c3d4.json":    "{}\n",        // a per-run transcript
		"notes.json":                       "{}\n",        // a real file — keep
		"20260605-143022-a1b2c3d4.json.go": "package x\n", // not a transcript — keep
	})
	out, err := toolListDir(context.Background(), sb, ".")
	if err != nil {
		t.Fatal(err)
	}
	listed := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		listed[strings.TrimSpace(strings.TrimPrefix(line, "file"))] = true
	}
	for _, hidden := range []string{"runs.jsonl", "20260605-143022-a1b2c3d4.json"} {
		if listed[hidden] {
			t.Errorf("list_dir leaked transcript artifact %q:\n%s", hidden, out)
		}
	}
	for _, kept := range []string{"main.go", "notes.json", "20260605-143022-a1b2c3d4.json.go"} {
		if !listed[kept] {
			t.Errorf("list_dir dropped real file %q:\n%s", kept, out)
		}
	}
}

func TestParseWriteArg(t *testing.T) {
	cases := []struct {
		in      string
		path    string
		content string
		bad     string
	}{
		{"notes.txt hello", "notes.txt", "hello", ""},
		{"notes.txt hello\\nworld", "notes.txt", "hello\nworld", ""}, // \n -> newline
		{"a.txt one\\ttwo", "a.txt", "one\ttwo", ""},                 // \t -> tab
		{`a.txt c:\\dir`, "a.txt", `c:\dir`, ""},                     // \\ -> literal backslash
		{"empty.txt", "empty.txt", "", ""},                           // path only -> empty file
		{"sub/f.go package main", "sub/f.go", "package main", ""},    // path with subdir, content with space
		{`a.txt bad\zescape`, "a.txt", "", `\z`},                     // botched escape (path still parsed)
		{`a.txt trailing\`, "a.txt", "", `\`},                        // lone trailing backslash
	}
	for _, c := range cases {
		p, content, bad := parseWriteArg(c.in)
		if p != c.path || content != c.content || bad != c.bad {
			t.Errorf("parseWriteArg(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, p, content, bad, c.path, c.content, c.bad)
		}
	}
}

func TestWriteFileRoundTrips(t *testing.T) {
	sb := sbWith(t, nil)
	ctx := context.Background()

	out, err := toolWriteFile(ctx, sb, "notes.txt line1\\nline2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "notes.txt") || !strings.Contains(out, "2 line(s)") {
		t.Errorf("confirmation = %q, want path + line count", out)
	}
	// read_file sees exactly what write_file wrote — the decode landed real newlines.
	got, err := toolReadFile(ctx, sb, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := "1| line1\n2| line2"; got != want {
		t.Errorf("round-trip read = %q, want %q", got, want)
	}
}

func TestWriteFileOverwrites(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.txt": "old contents here\n"})
	if _, err := toolWriteFile(context.Background(), sb, "f.txt new"); err != nil {
		t.Fatal(err)
	}
	got, _ := toolReadFile(context.Background(), sb, "f.txt")
	if got != "1| new" {
		t.Errorf("after overwrite read = %q, want the file replaced, not appended", got)
	}
}

func TestWriteFileEscapeOutsideRootRefused(t *testing.T) {
	sb := sbWith(t, nil)
	_, err := toolWriteFile(context.Background(), sb, "../escape.txt nope")
	if err == nil || !strings.Contains(err.Error(), "outside the sandbox root") {
		t.Errorf("escape write err = %v, want a fence refusal", err)
	}
}

func TestWriteFileMissingParentIsRecovery(t *testing.T) {
	sb := sbWith(t, nil)
	_, err := toolWriteFile(context.Background(), sb, "no/such/dir/f.txt body")
	if err == nil || !strings.Contains(err.Error(), "mkdir -p") {
		t.Errorf("missing-parent err = %v, want a `mkdir -p` recovery message", err)
	}
}

func TestWriteFileBadEscapeIsRecovery(t *testing.T) {
	sb := sbWith(t, nil)
	_, err := toolWriteFile(context.Background(), sb, `f.txt oops\zhere`)
	if err == nil || !strings.Contains(err.Error(), "invalid escape") {
		t.Errorf("bad-escape err = %v, want an 'invalid escape' recovery message", err)
	}
}

func TestEditFileMissingFileIsRecovery(t *testing.T) {
	sb := sbWith(t, nil)
	_, err := toolEditFile(context.Background(), sb, "nope.txt old ||| new")
	if err == nil || !strings.Contains(err.Error(), "list_dir") {
		t.Errorf("missing-file err = %v, want a list_dir pointer", err)
	}
}

func TestEditFileEscapeOutsideRootRefused(t *testing.T) {
	sb := sbWith(t, nil)
	_, err := toolEditFile(context.Background(), sb, "../escape.txt old ||| new")
	if err == nil || !strings.Contains(err.Error(), "outside the sandbox root") {
		t.Errorf("escape edit err = %v, want a fence refusal", err)
	}
}

func TestLooksLikeEditEnvelope(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"apply_patch begin", "*** Begin Patch\n*** Add File: calc.go\n", "*** Begin Patch"},
		{"leading whitespace", "  \n\t*** Begin Patch\n", "*** Begin Patch"},
		{"add file directive", "*** Add File: x.go\npackage main\n", "*** Add File:"},
		{"code fence", "```go\npackage main\n```", "```"},
		{"diff old header", "--- a/x.go\n+++ b/x.go\n", "--- "},
		{"diff new header", "+++ b/x.go\n", "+++ "},
		{"hunk header", "@@ -1,3 +1,4 @@\n", "@@ "},
		{"normal go", "package main\n\nfunc main() {}\n", ""},
		{"yaml front matter", "---\ntitle: x\n---\n", ""}, // "---\n" has no space -> not a diff header.
		{"regex content", `re := "\d+\.\d+"`, ""},
		{"empty", "", ""},
		{"fence midfile is fine", "package main\n```\n", ""}, // only a LEADING fence is rejected.
	}
	for _, c := range cases {
		if got := looksLikeEditEnvelope(c.in); got != c.want {
			t.Errorf("%s: looksLikeEditEnvelope(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestIsRunFailureAndFingerprint(t *testing.T) {
	// isRunFailure keys on the formatRun "exit <code> (" prefix.
	if isRunFailure("exit 0 (12ms)\nstdout:\nok") {
		t.Error("exit 0 must not be a failure")
	}
	if !isRunFailure("exit 2 (12ms)\nstderr:\nboom") {
		t.Error("exit 2 must be a failure")
	}
	if isRunFailure("wrote 3 byte(s) to f.txt") {
		t.Error("a non-run observation must not register as a run failure")
	}
	// runFingerprint strips the jittering duration so two identical failures match.
	a := runFingerprint("exit 2 (131ms)\nstderr:\nundefined: foo")
	b := runFingerprint("exit 2 (9ms)\nstderr:\nundefined: foo")
	if a != b {
		t.Errorf("fingerprints differ only by duration but did not match:\n%q\n%q", a, b)
	}
	if strings.Contains(a, "ms)") {
		t.Errorf("fingerprint still carries the duration: %q", a)
	}
	// A different exit code or body must NOT match.
	if a == runFingerprint("exit 1 (9ms)\nstderr:\nundefined: foo") {
		t.Error("different exit codes should not share a fingerprint")
	}
}

func TestWriteFileRejectsEditEnvelope(t *testing.T) {
	sb := sbWith(t, nil)
	ctx := context.Background()
	// The R10 gpt-5-nano failure: apply_patch envelope leaked into content.
	_, err := writeFileOp(ctx, sb, "calc.go", "*** Begin Patch\n*** Add File: calc.go\npackage main\n", false)
	if err == nil || !strings.Contains(err.Error(), "patch/diff/fence") {
		t.Errorf("apply_patch envelope err = %v, want a recovery message about a patch wrapper", err)
	}
	// And it must NOT have written the corrupt file.
	if _, rerr := sb.ReadFile(ctx, "calc.go"); rerr == nil {
		t.Error("rejected write still created the file")
	}
	// A leading markdown fence (R3-style wrapping) is likewise rejected.
	if _, err := writeFileOp(ctx, sb, "x.go", "```go\npackage main\n```", false); err == nil {
		t.Error("leading code fence should be rejected")
	}
	// Clean content still writes.
	if _, err := writeFileOp(ctx, sb, "ok.go", "package main\n", false); err != nil {
		t.Errorf("clean content was wrongly rejected: %v", err)
	}
}

func TestClipKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("H", 30) + strings.Repeat("T", 30) // 60 runes
	got := clip(s, 12)
	if !strings.HasPrefix(got, "H") || !strings.HasSuffix(got, "T") {
		t.Errorf("clip dropped head or tail: %q", got)
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("clip lacks an elision marker: %q", got)
	}
	// Under cap: untouched.
	if clip("short", 100) != "short" {
		t.Errorf("clip mangled under-cap input")
	}
}
