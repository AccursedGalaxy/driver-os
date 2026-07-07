package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

func TestReadOptions(t *testing.T) {
	// 1. Window respected: a 300-line file read with ReadOptions{Window:80} returns exactly
	// 80 numbered lines and a footer pointing to the next range starting at line 81.
	t.Run("window respected", func(t *testing.T) {
		var sb strings.Builder
		for i := 1; i <= 300; i++ {
			fmt.Fprintf(&sb, "line %d\n", i)
		}
		sbox := sbWithOutline(t, map[string]string{"test.txt": sb.String()})
		opts := ReadOptions{Window: 80}
		res, err := readFileOp(nil, sbox, "test.txt", 1, 0, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(res, "\n")
		// 80 lines + 1 footer line
		if len(lines) != 81 {
			t.Errorf("expected 81 lines, got %d", len(lines))
		}
		if !strings.Contains(lines[79], "80| line 80") {
			t.Errorf("expected line 80, got %q", lines[79])
		}
		if !strings.Contains(lines[80], "read `test.txt:81-160` for the next chunk") {
			t.Errorf("expected footer for next chunk, got %q", lines[80])
		}
	})

	// 2. Default preserved: the same read with the zero ReadOptions{} returns 150 lines
	// (unchanged) and NO outline text (assert the output does not contain "file outline").
	t.Run("default preserved", func(t *testing.T) {
		var sb strings.Builder
		for i := 1; i <= 300; i++ {
			fmt.Fprintf(&sb, "line %d\n", i)
		}
		sbox := sbWithOutline(t, map[string]string{"test.txt": sb.String()})
		opts := ReadOptions{}
		res, err := readFileOp(nil, sbox, "test.txt", 1, 0, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(res, "\n")
		if len(lines) != 151 {
			t.Errorf("expected 151 lines, got %d", len(lines))
		}
		if strings.Contains(res, "file outline") {
			t.Error("expected no file outline")
		}
	})

	// 3. Outline on, Go file, clipped: a >Window .go file with ReadOptions{Window:20,
	// Outline:true} appends the outline; assert it contains a known func/type symbol
	// with its correct line number.
	t.Run("outline on go file clipped", func(t *testing.T) {
		src := `package main
type Server struct{}
func (s *Server) Serve() {}
`
		for i := 0; i < 100; i++ {
			src += fmt.Sprintf("// line %d\n", i+4)
		}
		sbox := sbWithOutline(t, map[string]string{"main.go": src})
		opts := ReadOptions{Window: 10, Outline: true}
		res, err := readFileOp(nil, sbox, "main.go", 1, 0, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res, "--- file outline (2 symbols)") {
			t.Error("expected file outline header")
		}
		if !strings.Contains(res, "   2 type Server") {
			t.Error("expected type Server in outline")
		}
		if !strings.Contains(res, "   3 func (*Server) Serve") {
			t.Error("expected func Serve in outline")
		}
	})

	// 4. Outline NOT added when the whole file fits (a 10-line file with Outline:true and
	// Window:150 → no "file outline" text, since clippedLines==0).
	t.Run("no outline when fits", func(t *testing.T) {
		src := "package main\nfunc Foo() {}\n"
		sbox := sbWithOutline(t, map[string]string{"main.go": src})
		opts := ReadOptions{Window: 150, Outline: true}
		res, err := readFileOp(nil, sbox, "main.go", 1, 0, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res, "file outline") {
			t.Error("expected no file outline when file fits in window")
		}
	})

	// 5. Outline generic fallback: a non-Go file (e.g. x.py with def foo(): /
	// class Bar:) clipped with Outline:true → outline lists those with line numbers.
	t.Run("outline generic fallback", func(t *testing.T) {
		src := "class Bar:\n    pass\n\ndef foo():\n    pass\n"
		for i := 0; i < 100; i++ {
			src += fmt.Sprintf("# line %d\n", i+7)
		}
		sbox := sbWithOutline(t, map[string]string{"x.py": src})
		opts := ReadOptions{Window: 10, Outline: true}
		res, err := readFileOp(nil, sbox, "x.py", 1, 0, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res, "--- file outline (2 symbols)") {
			t.Error("expected file outline header")
		}
		if !strings.Contains(res, "   1 class Bar:") {
			t.Error("expected class Bar in outline")
		}
		if !strings.Contains(res, "   4 def foo():") {
			t.Error("expected def foo in outline")
		}
	})

	// 6. Outline cap: a Go file with >50 top-level funcs, clipped, Outline:true → outline shows
	// 50 entries and a (+N more symbols) line.
	t.Run("outline cap", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString("package main\n")
		for i := 1; i <= 60; i++ {
			fmt.Fprintf(&sb, "func Func%d() {}\n", i)
		}
		for i := 0; i < 100; i++ {
			sb.WriteString("// padding\n")
		}
		sbox := sbWithOutline(t, map[string]string{"large.go": sb.String()})
		opts := ReadOptions{Window: 10, Outline: true}
		res, err := readFileOp(nil, sbox, "large.go", 1, 0, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res, "--- file outline (60 symbols)") {
			t.Error("expected 60 symbols in header")
		}
		if !strings.Contains(res, "  … (+10 more symbols)") {
			t.Error("expected (+10 more symbols) line")
		}
		// Count occurrences of "func Func"
		// The window shows Func1 to Func9 (9 symbols).
		// The outline shows 50 symbols.
		// Total "func Func" should be 59.
		count := strings.Count(res, "func Func")
		if count != 59 {
			t.Errorf("expected 59 symbols shown (9 in window + 50 in outline), got %d", count)
		}
	})

	// 7. Go file with no symbols: a .go file with only package/imports and comments
	// should return NO outline (not fall back to generic scanner).
	t.Run("go file no symbols", func(t *testing.T) {
		src := "package main\nimport \"fmt\"\n"
		for i := 0; i < 200; i++ {
			src += "// comment\n"
		}
		sbox := sbWithOutline(t, map[string]string{"empty.go": src})
		opts := ReadOptions{Window: 10, Outline: true}
		res, err := readFileOp(nil, sbox, "empty.go", 1, 0, false, opts)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res, "file outline") {
			t.Error("expected no file outline for Go file with no symbols")
		}
	})
}

func sbWithOutline(t *testing.T, files map[string]string) sandbox.Sandbox {
	return sbWith(t, files)
}
