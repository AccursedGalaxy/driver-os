package agent

import (
	"context"
	"os"
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
	body, err := readFileOp(context.Background(), sbWith(t, nil), goFile, 1, 3, true, ReadOptions{})
	if err != nil {
		t.Fatalf("read_file %s: %v", goFile, err)
	}
	if !strings.Contains(body, "1| ") {
		t.Errorf("dependency source read is not line-numbered:\n%s", body)
	}
}

func TestSemverPick(t *testing.T) {
	dirs := []string{
		"/c/golang.org/x/sync@v0.9.0",
		"/c/golang.org/x/sync@v0.20.0",
		"/c/golang.org/x/sync@v0.18.0",
	}
	if got := highestVersionDir(dirs); got != "/c/golang.org/x/sync@v0.20.0" {
		t.Errorf("highestVersionDir = %q, want the v0.20.0 dir (v0.20 > v0.9 numerically, not lexically)", got)
	}
}

func TestDocPackageAndSymbol(t *testing.T) {
	cases := []struct {
		in       []string
		pkg, sym string
	}{
		{[]string{"strings"}, "strings", ""},
		{[]string{"strings.Builder"}, "strings", "Builder"},
		{[]string{"net/http", "Client"}, "net/http", "Client"},
		{[]string{"golang.org/x/sync/errgroup"}, "golang.org/x/sync/errgroup", ""},
		{[]string{"golang.org/x/sync/errgroup", "Group"}, "golang.org/x/sync/errgroup", "Group"},
	}
	for _, c := range cases {
		pkg, sym := docPackageAndSymbol(c.in)
		if pkg != c.pkg || sym != c.sym {
			t.Errorf("docPackageAndSymbol(%v) = (%q,%q), want (%q,%q)", c.in, pkg, sym, c.pkg, c.sym)
		}
	}
}

// TestGoDocCacheFallback is the fix: a package NOT in the current module's graph
// but present in the module cache must still resolve, via the cache fallback. Run
// from a bare temp module (no x/sync dependency) so the primary lookup fails and
// the fallback is exercised. Skip-guarded on errgroup being cached.
func TestGoDocCacheFallback(t *testing.T) {
	if cacheDirFor("golang.org/x/sync/errgroup") == "" {
		t.Skip("golang.org/x/sync/errgroup not in the module cache")
	}
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "go.mod"), []byte("module bare\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := goDocArg(context.Background(), bare, "golang.org/x/sync/errgroup")
	if err != nil {
		t.Fatalf("go_doc errgroup (not a dep, from cache): %v", err)
	}
	if !strings.Contains(out, "package errgroup") {
		t.Errorf("fallback did not return errgroup docs:\n%s", out)
	}
	if !strings.Contains(out, "module cache") {
		t.Errorf("fallback output should note it came from the module cache:\n%s", out)
	}
	if !strings.Contains(out, "source:") {
		t.Errorf("fallback should surface the on-disk source dir:\n%s", out)
	}
	// And a symbol lookup through the fallback:
	sym, err := goDocArg(context.Background(), bare, "golang.org/x/sync/errgroup Group")
	if err != nil {
		t.Fatalf("go_doc errgroup.Group from cache: %v", err)
	}
	if !strings.Contains(sym, "type Group") {
		t.Errorf("fallback symbol lookup did not return `type Group`:\n%s", sym)
	}
}

// TestGoDocTimeoutConst is a cheap guard that the lookup is bounded.
func TestGoDocTimeoutConst(t *testing.T) {
	if goDocTimeout <= 0 || goDocTimeout > time.Minute {
		t.Errorf("goDocTimeout = %v, want a small positive bound", goDocTimeout)
	}
}
