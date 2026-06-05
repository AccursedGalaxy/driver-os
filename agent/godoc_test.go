package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseGoDocArg(t *testing.T) {
	ok := []struct {
		arg    string
		flags  []string
		target []string
	}{
		{"strings", nil, []string{"strings"}},
		{"strings.Builder", nil, []string{"strings.Builder"}},
		{"net/http Client", nil, []string{"net/http", "Client"}},
		{"-src io.Copy", []string{"-src"}, []string{"io.Copy"}},
		{"-all -u net/http", []string{"-all", "-u"}, []string{"net/http"}},
		{"github.com/openai/openai-go ChatCompletionNewParams", nil, []string{"github.com/openai/openai-go", "ChatCompletionNewParams"}},
	}
	for _, c := range ok {
		flags, target, err := parseGoDocArg(c.arg)
		if err != nil {
			t.Errorf("parseGoDocArg(%q) error: %v", c.arg, err)
			continue
		}
		if strings.Join(flags, ",") != strings.Join(c.flags, ",") {
			t.Errorf("parseGoDocArg(%q) flags = %v, want %v", c.arg, flags, c.flags)
		}
		if strings.Join(target, ",") != strings.Join(c.target, ",") {
			t.Errorf("parseGoDocArg(%q) target = %v, want %v", c.arg, target, c.target)
		}
	}

	bad := []string{
		"",               // no package
		"-src",           // flag but no package
		"-bogus strings", // unknown flag
		"strings -src",   // flag after package
		"a b c",          // too many words
	}
	for _, arg := range bad {
		if _, _, err := parseGoDocArg(arg); err == nil {
			t.Errorf("parseGoDocArg(%q) = nil error, want a usage error", arg)
		}
	}
}

// TestGoDocStdlib exercises the real host `go doc` against the stdlib (always
// present, no module needed), so it's hermetic. It runs from the package dir,
// which is inside the driver-os module — a valid cwd for go doc.
func TestGoDocStdlib(t *testing.T) {
	out, err := goDocArg(context.Background(), ".", "strings.Builder")
	if err != nil {
		t.Fatalf("go_doc strings.Builder: %v", err)
	}
	if !strings.Contains(out, "Builder") {
		t.Errorf("go_doc strings.Builder did not mention Builder:\n%s", out)
	}
	// The bridge to file browsing: the on-disk source dir is surfaced.
	if !strings.Contains(out, "source:") {
		t.Errorf("go_doc output is missing the `source:` on-disk-path line:\n%s", out)
	}

	// -src shows the actual implementation, not just the doc comment.
	src, err := goDocArg(context.Background(), ".", "-src strings.Builder.WriteString")
	if err != nil {
		t.Fatalf("go_doc -src: %v", err)
	}
	if !strings.Contains(src, "func (b *Builder) WriteString") {
		t.Errorf("go_doc -src did not return the function source:\n%s", src)
	}
}

// TestGoDocDependencyEndToEnd is the headline use case: look up a real pinned
// DEPENDENCY's docs, follow the `source:` line into the module cache, and read its
// actual source with read_file — proving the full doc→browse flow on a dependency,
// not just stdlib. Skip-guarded so a dependency change can never make it fragile.
func TestGoDocDependencyEndToEnd(t *testing.T) {
	const dep = "github.com/openai/openai-go"
	// Resolve from the module root (walk up from the package dir to the go.mod).
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := goDocArg(context.Background(), root, dep)
	if err != nil {
		t.Skipf("dependency %s not resolvable here: %v", dep, err)
	}
	// Pull the on-disk dir off the `source:` line and confirm it's in the cache.
	srcDir := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "source: ") {
			srcDir = strings.TrimSpace(strings.TrimPrefix(line, "source: "))
		}
	}
	if srcDir == "" {
		t.Fatalf("go_doc %s produced no `source:` line:\n%s", dep, out)
	}
	if _, ok := goSourcePath(srcDir); !ok {
		t.Fatalf("source dir %q is not recognized as a read-only Go source path", srcDir)
	}
	// read_file must serve a file from that dependency directory host-side.
	entries, err := hostListDir(srcDir)
	if err != nil {
		t.Fatalf("listing %s: %v", srcDir, err)
	}
	var goFile string
	for _, e := range entries {
		if !e.IsDir && strings.HasSuffix(e.Name, ".go") {
			goFile = filepath.Join(srcDir, e.Name)
			break
		}
	}
	if goFile == "" {
		t.Fatalf("no .go file found in %s", srcDir)
	}
	body, err := readFileOp(context.Background(), sbWith(t, nil), goFile, 1, 3, true)
	if err != nil {
		t.Fatalf("read_file %s: %v", goFile, err)
	}
	if !strings.Contains(body, "1| ") {
		t.Errorf("dependency source read is not line-numbered:\n%s", body)
	}
}

// TestGoDocTimeoutConst is a cheap guard that the lookup is bounded.
func TestGoDocTimeoutConst(t *testing.T) {
	if goDocTimeout <= 0 || goDocTimeout > time.Minute {
		t.Errorf("goDocTimeout = %v, want a small positive bound", goDocTimeout)
	}
}
