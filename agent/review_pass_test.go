package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// errReviewer returns a canned error from Review (wrapping ErrReviewParse when
// parseErr is true), recording every call for assertions.
type errReviewer struct {
	parseErr bool
	calls    int
}

func (r *errReviewer) Review(_ context.Context, _ ReviewInput) (*ReviewVerdict, error) {
	r.calls++
	if r.parseErr {
		return nil, fmt.Errorf("%w: bad json", ErrReviewParse)
	}
	return nil, errors.New("reviewer down")
}

// initReviewPassGit creates a small git repo, returns its root dir, the HEAD
// tree hash, and a sandbox. The caller must close the sandbox.
func initReviewPassGit(t *testing.T) (root, baseTree string, sb *local.Sandbox) {
	t.Helper()
	root = t.TempDir()
	for name, body := range map[string]string{
		"calc.go":      "package calc\n",
		"calc_test.go": "package calc // test\n",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", "calc.go", "calc_test.go")
	runGit("-c", "user.name=test", "-c", "user.email=t@t", "commit", "-m", "base")

	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD^{tree}").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD^{tree}: %v\n%s", err, out)
	}
	baseTree = strings.TrimSpace(string(out))

	var lerr error
	sb, lerr = local.New(root)
	if lerr != nil {
		t.Fatal(lerr)
	}
	t.Cleanup(func() { sb.Close() })
	return root, baseTree, sb
}

func TestReviewAndRepairParseErrorReviewRequiredYieldsUnverified(t *testing.T) {
	root, baseTree, sb := initReviewPassGit(t)

	// Simulate solver's change so the review gate has a diff to review.
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc // patched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rv := &errReviewer{parseErr: true}
	base := &RunResult{Task: "fix calc", Root: root, Outcome: Answered, Answer: "done"}
	res, err := ReviewAndRepairExistingWorkspace(context.Background(), Config{
		Task:           "fix calc",
		Root:           root,
		Sandbox:        sb,
		Reviewer:       rv,
		ReviewRounds:   1,
		ReviewRequired: true,
	}, base, ReviewPassOptions{BaseTree: baseTree}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rv.calls != 1 {
		t.Fatalf("review calls = %d, want 1", rv.calls)
	}
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified", res.Outcome)
	}
	if !strings.Contains(res.Reason, "parse_error") {
		t.Fatalf("reason = %q, want parse_error", res.Reason)
	}
	if res.Review == nil || res.Review.Status != ReviewParseError {
		t.Fatalf("review report = %+v, want parse_error status", res.Review)
	}
	if res.Answer != "done" {
		t.Fatalf("answer = %q, want preserved", res.Answer)
	}
}

func TestReviewAndRepairParseErrorFailOpenStaysAnswered(t *testing.T) {
	root, baseTree, sb := initReviewPassGit(t)

	// Simulate solver's change so the review gate has a diff to review.
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc // patched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rv := &errReviewer{parseErr: true}
	base := &RunResult{Task: "fix calc", Root: root, Outcome: Answered, Answer: "done"}
	res, err := ReviewAndRepairExistingWorkspace(context.Background(), Config{
		Task:         "fix calc",
		Root:         root,
		Sandbox:      sb,
		Reviewer:     rv,
		ReviewRounds: 1,
		// ReviewRequired defaults to false → fail-open
	}, base, ReviewPassOptions{BaseTree: baseTree}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rv.calls != 1 {
		t.Fatalf("review calls = %d, want 1", rv.calls)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered (fail-open)", res.Outcome, res.Reason)
	}
	if res.Review == nil || res.Review.Status != ReviewParseError {
		t.Fatalf("review report = %+v, want parse_error status recorded", res.Review)
	}
	if res.Answer != "done" {
		t.Fatalf("answer = %q, want preserved", res.Answer)
	}
}