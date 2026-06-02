package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// TestSearchSurfacesRootDocs is the regression test for the bug this tool fixes:
// a search must reach the markdown docs at the repo ROOT (DESIGN.md, HARD-PROBLEMS.md)
// even though the matching term ALSO lives under code directories. The old failure
// was `run grep` pointed at code dirs only; here the model gives just a pattern and
// the tool searches from the root, so the docs are unmissable.
func TestSearchSurfacesRootDocs(t *testing.T) {
	requireRg(t)
	sb := sbWith(t, map[string]string{
		"DESIGN.md":        "We rely on the provider's own retry/backoff; do not add our own.\n",
		"HARD-PROBLEMS.md": "Open: should retry be configurable?\n",
		"agent/loop.go":    "// no retry here\nfunc loop() {}\n",
	})
	out, err := searchOp(context.Background(), sb, "retry", "", 10*time.Second)
	if err != nil {
		t.Fatalf("searchOp: %v", err)
	}
	for _, want := range []string{"DESIGN.md", "HARD-PROBLEMS.md", "agent/loop.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("search did not surface %s; got:\n%s", want, out)
		}
	}
}

// TestSearchRespectsGitignore proves the gitignore default: a matching term inside
// a gitignored directory (the _deps clones / eval trace dumps) is NOT returned, so
// the result is signal not noise — while the tracked file with the same term is.
// rg only honors .gitignore inside a git repo, which is the production scenario
// (the issue-bot runs against a real CI checkout), so the temp root is git-init'd.
func TestSearchRespectsGitignore(t *testing.T) {
	requireRg(t)
	sb := sbWith(t, map[string]string{
		".gitignore":       "_deps/\n",
		"DESIGN.md":        "retry policy lives here\n",
		"_deps/clone/x.go": "retry in a vendored clone\n",
	})
	gitInit(t, sb)
	out, err := searchOp(context.Background(), sb, "retry", "", 10*time.Second)
	if err != nil {
		t.Fatalf("searchOp: %v", err)
	}
	if !strings.Contains(out, "DESIGN.md") {
		t.Errorf("tracked doc missing from results:\n%s", out)
	}
	if strings.Contains(out, "_deps") {
		t.Errorf("gitignored dir leaked into results:\n%s", out)
	}
}

// TestSearchNoMatch checks the "confirmed absence is an answer" framing (P3), not a
// bare empty string.
func TestSearchNoMatch(t *testing.T) {
	requireRg(t)
	sb := sbWith(t, map[string]string{"a.go": "package a\n"})
	out, err := searchOp(context.Background(), sb, "nonexistent_term_xyz", "", 10*time.Second)
	if err != nil {
		t.Fatalf("searchOp: %v", err)
	}
	if !strings.Contains(out, "no matches") {
		t.Errorf("expected a no-matches answer, got:\n%s", out)
	}
}

// TestSearchEmptyPattern teaches the shape rather than running an empty search.
func TestSearchEmptyPattern(t *testing.T) {
	sb := sbWith(t, map[string]string{"a.go": "package a\n"})
	if _, err := searchOp(context.Background(), sb, "   ", "", 10*time.Second); err == nil {
		t.Fatal("expected an error for an empty pattern")
	}
}

// TestValidateSearchPath keeps an optional narrowing path inside the root.
func TestValidateSearchPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"agent", "agent", false},
		{"agent/", "agent", false},
		{"/etc", "", true},
		{"../escape", "", true},
		{"..", "", true},
	}
	for _, c := range cases {
		got, err := validateSearchPath(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSearchPath(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("validateSearchPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSearchInReadOnlyTools confirms the tool ships in BOTH toolsets — the
// read-only set (issue-bot) is exactly where the grep-scope bug was observed.
func TestSearchInReadOnlyTools(t *testing.T) {
	sb := sbWith(t, map[string]string{"a.go": "package a\n"})
	if _, ok := DefaultTools(sb, time.Second)["search"]; !ok {
		t.Error("search missing from DefaultTools")
	}
	if _, ok := ReadOnlyTools(sb, time.Second)["search"]; !ok {
		t.Error("search missing from ReadOnlyTools")
	}
}

func requireRg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed; skipping search integration test")
	}
}

// gitInit makes the sandbox root a git repo so rg honors .gitignore — rg only
// applies .gitignore rules inside a repository, which the real CI checkout always
// is. Run THROUGH the sandbox so it targets the sandbox's root, not the test cwd.
func gitInit(t *testing.T, sb sandbox.Sandbox) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; skipping gitignore search test")
	}
	res, err := sb.Exec(context.Background(), sandbox.Command{Path: "git", Args: []string{"init", "-q", "."}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("git init: err=%v exit=%d", err, res.ExitCode)
	}
}
