package agent

import (
	"context"
	"strings"
	"testing"
)

func TestEditFileInsertBeforeAndAfter(t *testing.T) {
	ctx := context.Background()

	t.Run("before", func(t *testing.T) {
		sb := sbWith(t, map[string]string{"f.go": "first\nanchor\nlast\n"})
		out, err := editFileOp(ctx, sb, "f.go", "anchor\n", "before\n", "insert_before")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := readback(t, sb, "f.go"), "first\nbefore\nanchor\nlast\n"; got != want {
			t.Fatalf("file = %q, want %q", got, want)
		}
		if !strings.Contains(out, "before") {
			t.Fatalf("echo does not cover insertion: %s", out)
		}
	})

	t.Run("after", func(t *testing.T) {
		sb := sbWith(t, map[string]string{"f.go": "first\nanchor\nlast\n"})
		out, err := editFileOp(ctx, sb, "f.go", "anchor\n", "after\n", "insert_after")
		if err != nil {
			t.Fatal(err)
		}
		if got, want := readback(t, sb, "f.go"), "first\nanchor\nafter\nlast\n"; got != want {
			t.Fatalf("file = %q, want %q", got, want)
		}
		if !strings.Contains(out, "after") {
			t.Fatalf("echo does not cover insertion: %s", out)
		}
	})
}

func TestEditFileInsertAnchorErrors(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, src, old, new, mode, want string
	}{
		{"ambiguous", "x\nx\n", "x", "new", "insert_before", "2 places"},
		{"missing", "x\n", "missing", "new", "insert_after", "not found"},
		{"empty new", "x\n", "x", "", "insert_before", "non-empty"},
		{"bad mode", "x\n", "x", "new", "beside", "invalid edit_file mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := sbWith(t, map[string]string{"f.go": tc.src})
			_, err := editFileOp(ctx, sb, "f.go", tc.old, tc.new, tc.mode)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEditFileInsertTextProtocolAndLegacyReplace(t *testing.T) {
	ctx := context.Background()
	sb := sbWith(t, map[string]string{"f.go": "a\nanchor\nz\n"})
	if _, err := toolEditFile(ctx, sb, `f.go anchor\n ||| inserted\n ||| insert_before`); err != nil {
		t.Fatalf("four-field insert: %v", err)
	}
	if got, want := readback(t, sb, "f.go"), "a\ninserted\nanchor\nz\n"; got != want {
		t.Fatalf("after text insert = %q, want %q", got, want)
	}

	// With three fields, mode defaults to replace exactly as it did before modes.
	if _, err := toolEditFile(ctx, sb, `f.go anchor ||| changed`); err != nil {
		t.Fatalf("three-field replace: %v", err)
	}
	if got, want := readback(t, sb, "f.go"), "a\ninserted\nchanged\nz\n"; got != want {
		t.Fatalf("after legacy replace = %q, want %q", got, want)
	}
}
