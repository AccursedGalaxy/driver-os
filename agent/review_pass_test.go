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

	"github.com/AccursedGalaxy/driver-os/llm"
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
		Task:         "fix calc",
		Root:         root,
		Sandbox:      sb,
		Reviewer:     rv,
		ReviewRounds: 1,
		ReviewPolicy: ReviewPolicyRequired,
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

func TestReviewAndRepairParseErrorDefaultRequiredYieldsUnverified(t *testing.T) {
	root, baseTree, sb := initReviewPassGit(t)
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
	}, base, ReviewPassOptions{BaseTree: baseTree}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s (%s), want unverified by default", res.Outcome, res.Reason)
	}
	if res.Review == nil || res.Review.Status != ReviewParseError {
		t.Fatalf("review report = %+v, want parse_error status", res.Review)
	}
}

func TestReviewFailOpenAndReviewRequiredIsSetupError(t *testing.T) {
	root, _, sb := initReviewPassGit(t)
	err := (Config{Model: &nativeScript{},
		Root:         root,
		Sandbox:      sb,
		Task:         "fix calc",
		Reviewer:     &errReviewer{},
		ReviewPolicy: ReviewPolicy(99),
	}).Validate()
	if err == nil {
		t.Fatal("Validate succeeded, want setup error")
	}
	if !strings.Contains(err.Error(), "ReviewPolicy") {
		t.Fatalf("error = %v, want conflict message", err)
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
		ReviewPolicy: ReviewPolicyFailOpen,
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

type scriptedReviewPassReviewer struct {
	verdicts []*ReviewVerdict
	inputs   []ReviewInput
}

func (r *scriptedReviewPassReviewer) Review(_ context.Context, in ReviewInput) (*ReviewVerdict, error) {
	r.inputs = append(r.inputs, in)
	idx := len(r.inputs) - 1
	if idx >= len(r.verdicts) {
		return &ReviewVerdict{Model: "fake-reviewer"}, nil
	}
	return r.verdicts[idx], nil
}

func TestReviewAndRepairReReviewsFullFinalDiffAndMergesReports(t *testing.T) {
	root, baseTree, sb := initReviewPassGit(t)
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc\n\nfunc Value() string { return \"broken\" }\nfunc Kept() string { return \"winner\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rv := &scriptedReviewPassReviewer{verdicts: []*ReviewVerdict{
		{
			Findings: []ReviewFinding{{File: "calc.go", Quote: "broken", Severity: "blocker", Confidence: 10, FailureScenario: "broken value remains"}},
			Usage:    llm.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
			Model:    "fake-reviewer",
			Summary:  "round one blocked",
			RunID:    "review-run-1",
		},
		{
			Usage:   llm.Usage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11},
			Model:   "fake-reviewer",
			Summary: "round two clean",
			RunID:   "review-run-2",
		},
	}}
	base := &RunResult{Task: "fix calc", Root: root, Outcome: Answered, Answer: "done", Usage: llm.Usage{PromptTokens: 100, TotalTokens: 100}}
	var repairReviewerNil bool
	res, err := ReviewAndRepairExistingWorkspace(context.Background(), Config{
		Task:         "fix calc",
		Root:         root,
		Sandbox:      sb,
		Reviewer:     rv,
		ReviewRounds: 2,
	}, base, ReviewPassOptions{BaseTree: baseTree}, func(_ context.Context, cfg Config) (*RunResult, error) {
		repairReviewerNil = cfg.Reviewer == nil
		if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc\n\nfunc Value() string { return \"fixed\" }\nfunc Kept() string { return \"winner\" }\n"), 0o644); err != nil {
			return nil, err
		}
		return &RunResult{Task: cfg.Task, Root: root, Outcome: Answered, Answer: "repaired", Usage: llm.Usage{PromptTokens: 20, TotalTokens: 20}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !repairReviewerNil {
		t.Fatalf("repair loop ran with reviewer enabled")
	}
	if len(rv.inputs) != 2 {
		t.Fatalf("review calls = %d, want 2", len(rv.inputs))
	}
	if rv.inputs[0].SessionKey == "" || rv.inputs[1].SessionKey != rv.inputs[0].SessionKey {
		t.Fatalf("session keys = %q, %q; want stable non-empty", rv.inputs[0].SessionKey, rv.inputs[1].SessionKey)
	}
	if rv.inputs[0].Round != 1 || rv.inputs[1].Round != 2 {
		t.Fatalf("rounds = %d, %d; want 1, 2", rv.inputs[0].Round, rv.inputs[1].Round)
	}
	if strings.Contains(rv.inputs[1].Diff, "broken") {
		t.Fatalf("second review diff still contains pre-repair broken code:\n%s", rv.inputs[1].Diff)
	}
	if !strings.Contains(rv.inputs[1].Diff, "fixed") || !strings.Contains(rv.inputs[1].Diff, "Kept") {
		t.Fatalf("second review diff = %q, want full final diff against original base with fixed and kept changes", rv.inputs[1].Diff)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered", res.Outcome, res.Reason)
	}
	if res.Usage.PromptTokens != 120 || res.Usage.TotalTokens != 120 {
		t.Fatalf("usage = %+v, want winner and repair solve usage folded", res.Usage)
	}
	if res.Review == nil {
		t.Fatal("missing merged review report")
	}
	if res.Review.Rounds != 2 || res.Review.Status != ReviewClean || res.Review.Blocked {
		t.Fatalf("review report status = rounds %d status %s blocked %v, want 2 clean false", res.Review.Rounds, res.Review.Status, res.Review.Blocked)
	}
	if res.Review.Usage.PromptTokens != 13 || res.Review.Usage.TotalTokens != 18 {
		t.Fatalf("review usage = %+v, want both passes", res.Review.Usage)
	}
	if got := strings.Join(res.Review.ReviewerRuns, ","); got != "review-run-1,review-run-2" {
		t.Fatalf("reviewer runs = %q", got)
	}
	if got := strings.Join(res.Review.Summaries, "|"); got != "round one blocked|round two clean" {
		t.Fatalf("summaries = %q", got)
	}
	if len(res.Review.Findings) != 1 {
		t.Fatalf("findings = %d, want round-1 blocker retained", len(res.Review.Findings))
	}
	f := res.Review.Findings[0]
	if f.Round != 1 || f.Quote != "broken" || f.Fate != FateRepaired {
		t.Fatalf("round-1 finding = %+v, want retained with repaired fate", f)
	}
}

func TestReviewAndRepairSurvivingBlockersAfterAllRoundsUnverified(t *testing.T) {
	root, baseTree, sb := initReviewPassGit(t)
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc\n\nfunc Value() string { return \"broken\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockerFinding := ReviewFinding{File: "calc.go", Quote: "broken", Severity: "blocker", Confidence: 10, FailureScenario: "broken value remains"}
	rv := &scriptedReviewPassReviewer{verdicts: []*ReviewVerdict{
		{Findings: []ReviewFinding{blockerFinding}, Usage: llm.Usage{TotalTokens: 7}, Model: "fake-reviewer"},
		{Findings: []ReviewFinding{blockerFinding}, Usage: llm.Usage{TotalTokens: 11}, Model: "fake-reviewer"},
	}}
	base := &RunResult{Task: "fix calc", Root: root, Outcome: Answered, Answer: "done"}
	res, err := ReviewAndRepairExistingWorkspace(context.Background(), Config{
		Task:         "fix calc",
		Root:         root,
		Sandbox:      sb,
		Reviewer:     rv,
		ReviewRounds: 2,
	}, base, ReviewPassOptions{BaseTree: baseTree}, func(_ context.Context, cfg Config) (*RunResult, error) {
		if cfg.Reviewer != nil {
			t.Fatalf("repair loop reviewer enabled")
		}
		return &RunResult{Task: cfg.Task, Root: root, Outcome: Answered, Answer: "still broken"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rv.inputs) != 2 {
		t.Fatalf("review calls = %d, want 2", len(rv.inputs))
	}
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified", res.Outcome)
	}
	if !strings.Contains(res.Reason, "review blockers remain after 2 round(s)") {
		t.Fatalf("reason = %q", res.Reason)
	}
	if res.Review == nil || res.Review.Rounds != 2 || res.Review.Status != ReviewBlocked || !res.Review.Blocked {
		t.Fatalf("review report = %+v, want blocked after 2 rounds", res.Review)
	}
	if len(res.Review.Findings) != 2 {
		t.Fatalf("findings = %d, want both review passes retained", len(res.Review.Findings))
	}
	if res.Review.Findings[0].Round != 1 || res.Review.Findings[0].Fate != FateRepaired {
		t.Fatalf("first finding = %+v, want round 1 repaired", res.Review.Findings[0])
	}
	if res.Review.Findings[1].Round != 2 || res.Review.Findings[1].Fate != FateExpired {
		t.Fatalf("second finding = %+v, want round 2 expired", res.Review.Findings[1])
	}
}
