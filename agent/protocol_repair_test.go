package agent

// Text-loop protocol repair (measured: lab docs/findings/PROTOCOL-REPAIR-REPLAY.md,
// n=43 recorded local-model red steps). Local models wrap edit_file's OLD/NEW in
// quotes/backticks, over-escape inner quotes as \", and occasionally quote the
// ||| separator. Every repair is fail-closed: it fires only when the raw form
// FAILS and the normalized form matches the file EXACTLY once — a wrong silent
// edit is impossible by construction; ambiguity keeps today's error.

import (
	"context"
	"strings"
	"testing"
)

// --- R3: symmetric wrapper-strip (the dominant recorded class, 12/12
// state-validated in replay) -------------------------------------------------

func TestEditFileRepairsQuoteWrappedOldNew(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"percent.go": "package shop\n\nfunc percentOf(amount int64, pct int) int64 {\n\treturn amount / 100 * int64(pct)\n}\n",
	})
	// exact shape from repofix trace t1 step 6: OLD and NEW both "-wrapped
	arg := `percent.go "\treturn amount / 100 * int64(pct)" ||| "\treturn amount * int64(pct) / 100"`
	out, err := toolEditFile(context.Background(), sb, arg)
	if err != nil {
		t.Fatalf("wrapper-wrapped edit should repair and apply, got error: %v", err)
	}
	if !strings.Contains(out, "note:") {
		t.Errorf("repair must be transparent — observation should carry a note, got: %q", out)
	}
	body := readback(t, sb, "percent.go")
	if !strings.Contains(body, "amount * int64(pct) / 100") {
		t.Errorf("file not repaired: %q", body)
	}
	if strings.Contains(body, `"`) && strings.Contains(body, `/ 100 *`) {
		t.Errorf("stray quotes or unapplied edit in file: %q", body)
	}
}

func TestEditFileRepairsBacktickWrappedOldNew(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"percent.go": "package shop\n\nfunc percentOf(amount int64, pct int) int64 {\n\treturn amount / 100 * int64(pct)\n}\n",
	})
	arg := "percent.go `return amount / 100 * int64(pct)` ||| `return amount * int64(pct) / 100`"
	if _, err := toolEditFile(context.Background(), sb, arg); err != nil {
		t.Fatalf("backtick-wrapped edit should repair and apply, got error: %v", err)
	}
	if body := readback(t, sb, "percent.go"); !strings.Contains(body, "amount * int64(pct) / 100") {
		t.Errorf("file not repaired: %q", body)
	}
}

// Raw match must always win: if the file genuinely contains the quoted form,
// no stripping happens.
func TestEditFileRawMatchBeatsWrapperStrip(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"cfg.go": "package cfg\n\nvar level = \"debug\"\n",
	})
	// OLD includes the real quotes; must edit as-is, no repair note.
	arg := `cfg.go "debug" ||| "info"`
	out, err := toolEditFile(context.Background(), sb, arg)
	if err != nil {
		t.Fatalf("raw match should apply untouched: %v", err)
	}
	if strings.Contains(out, "note:") {
		t.Errorf("no repair should fire when the raw form matches, got: %q", out)
	}
	if body := readback(t, sb, "cfg.go"); !strings.Contains(body, `"info"`) {
		t.Errorf("file not edited: %q", body)
	}
}

// Fail-closed: stripped form absent from the file too — keep an error, never
// guess.
func TestEditFileWrapperStripFailsClosedOnNoMatch(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"a.go": "package a\n\nvar x = 1\n",
	})
	arg := `a.go "var y = 2" ||| "var y = 3"`
	if _, err := toolEditFile(context.Background(), sb, arg); err == nil {
		t.Fatal("stripped form absent — must still error, not guess")
	}
}

// Fail-closed: stripped form matches more than once — ambiguous, keep error.
func TestEditFileWrapperStripFailsClosedOnMultiMatch(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"b.go": "package b\n\nvar x = 1\nvar x2 = 1\n",
	})
	arg := `b.go "= 1" ||| "= 2"`
	if _, err := toolEditFile(context.Background(), sb, arg); err == nil {
		t.Fatal("stripped form ambiguous (2 matches) — must error, not guess")
	}
}

// Mode segment survives the repair.
func TestEditFileWrapperStripPreservesMode(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"m.go": "package m\n\nfunc f() {}\n",
	})
	arg := `m.go "func f() {}" ||| "// f does nothing.\n" ||| insert_before`
	if _, err := toolEditFile(context.Background(), sb, arg); err != nil {
		t.Fatalf("wrapped insert_before should repair and apply: %v", err)
	}
	body := readback(t, sb, "m.go")
	if !strings.Contains(body, "// f does nothing.") || !strings.Contains(body, "func f() {}") {
		t.Errorf("insert_before not applied through repair: %q", body)
	}
}

// --- R4: over-escaped inner quotes (\" -> ") — the class that killed rung-0
// run #4 (4 consecutive iterations) ------------------------------------------

func TestEditFileRepairsOverEscapedQuotes(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"oc.go": "package oc\n\nfunc is(m string) bool {\n\treturn contains(m, \"context length\")\n}\n",
	})
	// exact shape from run 20260717-111221: "-wrapped AND \" inner escapes
	arg := `oc.go "\treturn contains(m, \"context length\")" ||| "\treturn contains(m, \"context length\") || contains(m, \"reduce the length\")"`
	if _, err := toolEditFile(context.Background(), sb, arg); err != nil {
		t.Fatalf("over-escaped edit should repair and apply, got: %v", err)
	}
	if body := readback(t, sb, "oc.go"); !strings.Contains(body, `"reduce the length"`) {
		t.Errorf("file not repaired: %q", body)
	}
}

func TestEditFileOverEscapeFailsClosedOnNoMatch(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"oc.go": "package oc\n\nvar x = 1\n",
	})
	arg := `oc.go "contains(m, \"nope\")" ||| "contains(m, \"never\")"`
	if _, err := toolEditFile(context.Background(), sb, arg); err == nil {
		t.Fatal("unescaped-quote form absent — must error, not guess")
	}
}

// --- R2: quoted separator token ---------------------------------------------

func TestEditFileAcceptsQuotedSeparator(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"s.go": "package s\n\nvar n = 10\n",
	})
	arg := `s.go var n = 10 "|||" var n = 20`
	if _, err := toolEditFile(context.Background(), sb, arg); err != nil {
		t.Fatalf("quoted separator should be accepted: %v", err)
	}
	if body := readback(t, sb, "s.go"); !strings.Contains(body, "var n = 20") {
		t.Errorf("file not edited: %q", body)
	}
}

// Truly-missing separator stays an error (ambiguous by design).
func TestEditFileMissingSeparatorStillErrors(t *testing.T) {
	sb := sbWith(t, map[string]string{
		"s.go": "package s\n\nvar n = 10\n",
	})
	if _, err := toolEditFile(context.Background(), sb, "s.go var n = 10 var n = 20"); err == nil {
		t.Fatal("missing separator must remain an error")
	}
}
