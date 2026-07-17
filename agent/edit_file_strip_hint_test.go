package agent

import (
	"context"
	"strings"
	"testing"
)

// Anchoring-screen follow-ups (lab ANCHORING-HARNESS-SCREEN.md, 2026-07-17).
// Two deterministic upgrades to the edit_file anchoring path:
//
//  1. ASYMMETRIC WRAPPER STRIP (text loop): both live-leg residual not-found
//     failures had `old` quote-wrapped and `new` bare —
//     `percent.go "return amount / 100 * int64(pct)" ||| return amount * ...`
//     — which the symmetric-only strip refuses. Stripping `old` alone (after
//     the raw form was absent, unique-match validated as today) repairs them.
//  2. NEAREST-MATCH HINT (both loops): the `old`-not-found error names no
//     location; every model retried blind. When a window of the file is
//     token-similar to `old`, the error names its line(s) and echoes the
//     actual text so one read-free retry can succeed.

// --- 1. asymmetric strip -----------------------------------------------------

func TestEditFileTextProtocolStripsOldOnlyWrapper(t *testing.T) {
	// The exact live-sanity failure shape: quoted old, bare new.
	sb := sbWith(t, map[string]string{"percent.go": "func pct() int64 {\n\treturn amount / 100 * int64(pct)\n}\n"})
	out, err := toolEditFile(context.Background(), sb,
		`percent.go "return amount / 100 * int64(pct)" ||| return amount * int64(pct) / 100`)
	if err != nil {
		t.Fatalf("old-only wrapped edit should repair via strip: %v", err)
	}
	want := "func pct() int64 {\n\treturn amount * int64(pct) / 100\n}\n"
	if got := readback(t, sb, "percent.go"); got != want {
		t.Errorf("after old-only-strip edit = %q, want %q", got, want)
	}
	if !strings.Contains(out, "note:") || !strings.Contains(out, "stripped") {
		t.Errorf("repair must announce itself with a strip note; got:\n%s", out)
	}
}

func TestEditFileTextProtocolStripOldOnlyFailsClosed(t *testing.T) {
	// Stripped old still absent -> the ORIGINAL not-found error, file untouched.
	sb := sbWith(t, map[string]string{"f.go": "alpha\n"})
	_, err := toolEditFile(context.Background(), sb, `f.go "zzz" ||| yyy`)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want the original not-found error; got %v", err)
	}
	if got := readback(t, sb, "f.go"); got != "alpha\n" {
		t.Errorf("file changed on a failed repair: %q", got)
	}
}

func TestEditFileTextProtocolRawQuoteStillWins(t *testing.T) {
	// A literally-quoted anchor that EXISTS must be edited raw — no strip, no note.
	sb := sbWith(t, map[string]string{"f.go": "s := \"foo\"\nfoo := 1\n"})
	out, err := toolEditFile(context.Background(), sb, `f.go "foo" ||| "bar"`)
	if err != nil {
		t.Fatalf("raw quoted edit: %v", err)
	}
	if got, want := readback(t, sb, "f.go"), "s := \"bar\"\nfoo := 1\n"; got != want {
		t.Errorf("raw form must win before any strip: %q, want %q", got, want)
	}
	if strings.Contains(out, "stripped") {
		t.Errorf("raw success must not carry a strip note; got:\n%s", out)
	}
}

func TestEditFileTextProtocolStripOldOnlyAmbiguousFailsClosed(t *testing.T) {
	// Stripped old matching 2+ places keeps the failure (unique-match gate).
	sb := sbWith(t, map[string]string{"f.go": "x := 1\ny := 2\nx := 1\n"})
	_, err := toolEditFile(context.Background(), sb, `f.go "x := 1" ||| x := 9`)
	if err == nil {
		t.Fatal("ambiguous stripped match must not edit")
	}
	if got := readback(t, sb, "f.go"); got != "x := 1\ny := 2\nx := 1\n" {
		t.Errorf("file changed despite ambiguous stripped match: %q", got)
	}
}

// --- 2. nearest-match hint ---------------------------------------------------

func TestEditFileNotFoundHintsNearestMatch(t *testing.T) {
	// One token off (int32 vs int64): the error must point at the real line.
	sb := sbWith(t, map[string]string{"percent.go": "func pct() int64 {\n\treturn amount / 100 * int64(pct)\n}\n"})
	_, err := editFileOp(context.Background(), sb, "percent.go",
		"\treturn amount / 100 * int32(pct)", "\treturn amount * int32(pct) / 100")
	if err == nil {
		t.Fatal("want a not-found error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "closest match") {
		t.Fatalf("not-found error should carry a closest-match hint; got %v", err)
	}
	if !strings.Contains(msg, "line 2") {
		t.Errorf("hint should name the line of the nearest match; got %v", err)
	}
	if !strings.Contains(msg, "return amount / 100 * int64(pct)") {
		t.Errorf("hint should echo the actual file text; got %v", err)
	}
}

func TestEditFileNotFoundNoHintWhenDissimilar(t *testing.T) {
	// Nothing similar in the file -> the plain error, no misleading hint.
	sb := sbWith(t, map[string]string{"f.go": "package main\n\nfunc main() {}\n"})
	_, err := editFileOp(context.Background(), sb, "f.go",
		"completely unrelated zebra text", "x")
	if err == nil {
		t.Fatal("want a not-found error")
	}
	if strings.Contains(err.Error(), "closest match") {
		t.Errorf("dissimilar old must not produce a hint; got %v", err)
	}
}

func TestEditFileNotFoundHintMultiline(t *testing.T) {
	// A multi-line old with one drifted line still localizes the region.
	src := "a()\nb()\nc := compute(x)\nd()\ne()\n"
	sb := sbWith(t, map[string]string{"f.go": src})
	_, err := editFileOp(context.Background(), sb, "f.go",
		"b()\nc := compute(y)\nd()", "b()\nc := compute(z)\nd()")
	if err == nil {
		t.Fatal("want a not-found error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "closest match") || !strings.Contains(msg, "compute(x)") {
		t.Errorf("multi-line hint should echo the drifted region; got %v", err)
	}
	if got := readback(t, sb, "f.go"); got != src {
		t.Errorf("file changed on a hint-only path: %q", got)
	}
}
