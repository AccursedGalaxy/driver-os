package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

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
