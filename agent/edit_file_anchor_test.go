package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// edit_file is the anchor-based (drift-free) editor: it replaces the UNIQUE
// occurrence of `old` with `new`, identified by content rather than line numbers.
// These tests pin the contract the eval found we needed (HP-7): line-range edits
// drift when a model batches several edits without re-reading; content anchors
// can't drift because there are no numbers to go stale.

func TestEditFileReplace(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.go": "package main\n\nconst A = 1\n"})
	out, err := editFileOp(context.Background(), sb, "f.go", "const A = 1", "const A = 100")
	if err != nil {
		t.Fatalf("strReplaceOp: %v", err)
	}
	if got, want := readback(t, sb, "f.go"), "package main\n\nconst A = 100\n"; got != want {
		t.Errorf("after replace = %q, want %q", got, want)
	}
	if !strings.Contains(out, "const A = 100") { // re-grounding echo (P4)
		t.Errorf("confirmation should echo the changed region; got:\n%s", out)
	}
}

func TestEditFileAnchorNotFound(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.go": "package main\n"})
	_, err := editFileOp(context.Background(), sb, "f.go", "const A = 1", "const A = 2")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error that teaches the fix; got %v", err)
	}
}

func TestEditFileAmbiguousMatch(t *testing.T) {
	// Two identical lines: the model must add context to disambiguate, not guess.
	sb := sbWith(t, map[string]string{"f.go": "x := 1\ny := 1\nx := 1\n"})
	_, err := editFileOp(context.Background(), sb, "f.go", "x := 1", "x := 2")
	if err == nil || !strings.Contains(err.Error(), "2 places") {
		t.Fatalf("want an ambiguity error naming the match count; got %v", err)
	}
	// File must be untouched when the match is ambiguous.
	if got := readback(t, sb, "f.go"); got != "x := 1\ny := 1\nx := 1\n" {
		t.Errorf("file changed despite ambiguous match: %q", got)
	}
}

func TestEditFileDelete(t *testing.T) {
	// new == "" is an EXPLICIT deletion (unlike edit_file, where an OMITTED content
	// field silently deletes — the bug grok-4.3 fell into 21/21 edits).
	sb := sbWith(t, map[string]string{"f.go": "a\nDROP ME\nb\n"})
	if _, err := editFileOp(context.Background(), sb, "f.go", "DROP ME\n", ""); err != nil {
		t.Fatalf("strReplaceOp delete: %v", err)
	}
	if got, want := readback(t, sb, "f.go"), "a\nb\n"; got != want {
		t.Errorf("after delete = %q, want %q", got, want)
	}
}

func TestEditFileIdenticalRejected(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.go": "x\n"})
	if _, err := editFileOp(context.Background(), sb, "f.go", "x", "x"); err == nil {
		t.Fatal("replacing text with itself should error, not write a no-op")
	}
}

func TestEditFileEmptyOldRejected(t *testing.T) {
	// Empty `old` would match everywhere; steer to write_file instead.
	sb := sbWith(t, map[string]string{"f.go": "x\n"})
	if _, err := editFileOp(context.Background(), sb, "f.go", "", "y"); err == nil {
		t.Fatal("empty old should error")
	}
}

func TestEditFilePreservesTrailingNewlineConvention(t *testing.T) {
	// No trailing newline in, none out — the raw splice leaves bytes outside the
	// match untouched (no trailing-newline guesswork).
	sb := sbWith(t, map[string]string{"f.go": "alpha"})
	if _, err := editFileOp(context.Background(), sb, "f.go", "alpha", "beta"); err != nil {
		t.Fatalf("strReplaceOp: %v", err)
	}
	if got, want := readback(t, sb, "f.go"), "beta"; got != want {
		t.Errorf("trailing-newline convention not preserved: %q, want %q", got, want)
	}
}

func TestEditFileRejectsEditEnvelope(t *testing.T) {
	// Same guard as write_file/edit_file: a leaked apply_patch/diff/fence wrapper in
	// `new` is a protocol error, not file content (R3/R8/R10 lineage).
	sb := sbWith(t, map[string]string{"f.go": "x\n"})
	if _, err := editFileOp(context.Background(), sb, "f.go", "x", "*** Begin Patch\n+y\n"); err == nil {
		t.Fatal("a patch-envelope replacement should be rejected")
	}
}

// TestEditFileNoDriftAcrossEdits is the regression test for the gemini-3-flash
// failure: two surgical edits in one run, the FIRST adding a line (shifting every
// line below it). A line-range edit_file would need a re-read or the second edit
// drifts; str_replace's second edit still finds its anchor because it carries no
// line numbers. This is the whole point of the tool.
func TestEditFileNoDriftAcrossEdits(t *testing.T) {
	src := "package main\n\nconst A = 1\nconst B = 2\n\nfunc main() {}\n"
	turns := [][]llm.ContentPart{
		// Edit 1 inserts a line above B — under line-range editing this invalidates
		// B's line number for any not-yet-re-read edit.
		{structuredCall("c1", "edit_file", map[string]any{
			"path": "f.go", "old": "const A = 1", "new": "const A = 1\nconst A2 = 11"})},
		// Edit 2 targets B by CONTENT — unaffected by edit 1's line shift.
		{structuredCall("c2", "edit_file", map[string]any{
			"path": "f.go", "old": "const B = 2", "new": "const B = 200"})},
		{llm.Text("done")},
	}
	res, _, sb := runNative(t, map[string]string{"f.go": src}, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	want := "package main\n\nconst A = 1\nconst A2 = 11\nconst B = 200\n\nfunc main() {}\n"
	if got := readback(t, sb, "f.go"); got != want {
		t.Errorf("after two anchor edits =\n%q\nwant\n%q", got, want)
	}
}

// TestEditFileTextProtocol exercises the one-line text fallback grammar
// (`<path> <old> ||| <new>`) that non-native providers use.
func TestEditFileTextProtocol(t *testing.T) {
	sb := sbWith(t, map[string]string{"f.go": "const A = 1\n"})
	if _, err := toolEditFile(context.Background(), sb, `f.go const A = 1 ||| const A = 9`); err != nil {
		t.Fatalf("toolStrReplace: %v", err)
	}
	if got, want := readback(t, sb, "f.go"), "const A = 9\n"; got != want {
		t.Errorf("text-protocol str_replace = %q, want %q", got, want)
	}
}
