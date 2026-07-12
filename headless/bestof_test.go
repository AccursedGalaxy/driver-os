package headless

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/runspec"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// stubSelector is a deterministic bestOfSelector for testing selection logic.
// It returns the canned picks/reasons/errs in order, one per Compare call.
type stubSelector struct {
	picks   []string // "A" or "B" per call
	reasons []string
	errs    []error
	call    int
}

func (s *stubSelector) Compare(_ context.Context, task, patchA, patchB string) (string, string, llm.Usage, error) {
	i := s.call
	s.call++
	pick := "A"
	reason := ""
	var err error
	if i < len(s.picks) {
		pick = s.picks[i]
	}
	if i < len(s.reasons) {
		reason = s.reasons[i]
	}
	if i < len(s.errs) && s.errs[i] != nil {
		err = s.errs[i]
	}
	usage := llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	return pick, reason, usage, err
}

func makeAttempt(idx int, outcome agent.Outcome, patchNonEmpty bool) attemptRecord {
	return attemptRecord{
		Index:         idx,
		Outcome:       outcome,
		ExitCode:      0,
		PatchNonEmpty: patchNonEmpty,
		Usage:         llm.Usage{PromptTokens: 100, TotalTokens: 100},
	}
}

func TestSelectBestOf_ZeroNonEmptyPatch(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.KilledSpiral, false),
		makeAttempt(2, agent.KilledRepeat, false),
	}
	patches := []string{"", ""}
	sel := &stubSelector{}

	winner, report := selectBestOf(context.Background(), attempts, patches, "test task", sel, "select-model")
	if winner != 1 {
		t.Errorf("winner = %d, want 1 (last attempt when no patch produced)", winner)
	}
	if len(report.Verdicts) != 0 {
		t.Errorf("verdicts = %d, want 0 (no selector calls)", len(report.Verdicts))
	}
	if sel.call != 0 {
		t.Errorf("selector called %d times, want 0", sel.call)
	}
}

func TestSelectBestOf_OneNonEmptyPatch(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.KilledSpiral, false),
		makeAttempt(2, agent.Answered, true),
	}
	patches := []string{"", "diff content"}
	sel := &stubSelector{}

	winner, report := selectBestOf(context.Background(), attempts, patches, "test task", sel, "select-model")
	if winner != 1 {
		t.Errorf("winner = %d, want 1 (the only non-empty patch)", winner)
	}
	if len(report.Verdicts) != 0 {
		t.Errorf("verdicts = %d, want 0", len(report.Verdicts))
	}
	if sel.call != 0 {
		t.Errorf("selector called %d times, want 0", sel.call)
	}
}

func TestSelectBestOf_ConsistentPick(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.Answered, true),
		makeAttempt(2, agent.Answered, true),
	}
	patches := []string{"patch1", "patch2"}
	// Round 1: A=patch1, B=patch2 -> pick "A" (patch1)
	// Round 2: A=patch2, B=patch1 -> pick "B" (patch1)
	sel := &stubSelector{picks: []string{"A", "B"}, reasons: []string{"patch1 is better", "still patch1"}}

	winner, report := selectBestOf(context.Background(), attempts, patches, "test task", sel, "select-model")
	if winner != 0 {
		t.Errorf("winner = %d, want 0 (consistent pick for patch1)", winner)
	}
	if sel.call != 2 {
		t.Errorf("selector called %d times, want 2 (one comparison = 2 calls)", sel.call)
	}
	if len(report.Verdicts) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(report.Verdicts))
	}
	vd := report.Verdicts[0]
	if !vd.Consistent || vd.WinnerIndex != 0 {
		t.Errorf("verdict: consistent=%v winner=%d, want true, 0", vd.Consistent, vd.WinnerIndex)
	}
	if vd.PickRound1 != "A" || vd.PickRound2 != "B" {
		t.Errorf("picks = (%q, %q), want (A, B)", vd.PickRound1, vd.PickRound2)
	}
	if report.SelectorModel != "select-model" {
		t.Errorf("selector model = %q", report.SelectorModel)
	}
}

func TestSelectBestOf_ConsistentPickOtherWay(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.Answered, true),
		makeAttempt(2, agent.Answered, true),
	}
	patches := []string{"patch1", "patch2"}
	sel := &stubSelector{picks: []string{"B", "A"}, reasons: []string{"p2", "p2"}}

	winner, report := selectBestOf(context.Background(), attempts, patches, "test task", sel, "m")
	if winner != 1 {
		t.Errorf("winner = %d, want 1", winner)
	}
	vd := report.Verdicts[0]
	if !vd.Consistent || vd.WinnerIndex != 1 {
		t.Errorf("verdict: consistent=%v winner=%d, want true, 1", vd.Consistent, vd.WinnerIndex)
	}
}

func TestSelectBestOf_InconsistentPick_FallbackStructural(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.Answered, true),
		makeAttempt(2, agent.Answered, true),
	}
	patches := []string{"patch1", "patch2"}
	// Round1="A", Round2="A" -> same label, different underlying -> inconsistent
	sel := &stubSelector{picks: []string{"A", "A"}, reasons: []string{"r1", "r2"}}

	winner, report := selectBestOf(context.Background(), attempts, patches, "test task", sel, "m")
	if winner != 0 {
		t.Errorf("winner = %d, want 0 (structural tie -> earliest index)", winner)
	}
	vd := report.Verdicts[0]
	if vd.Consistent {
		t.Error("verdict should be inconsistent")
	}
	if vd.WinnerIndex != 0 {
		t.Errorf("WinnerIndex = %d, want 0", vd.WinnerIndex)
	}
	if !strings.Contains(vd.Reason, "r1") {
		t.Errorf("reason should contain round 1 reason: %q", vd.Reason)
	}
}

func TestSelectBestOf_StructuralFallbackAnsweredBeatsHitCap(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.HitCap, true),
		makeAttempt(2, agent.Answered, true),
	}
	patches := []string{"p1", "p2"}
	sel := &stubSelector{picks: []string{"A", "A"}} // inconsistent

	winner, _ := selectBestOf(context.Background(), attempts, patches, "test task", sel, "m")
	if winner != 1 {
		t.Errorf("winner = %d, want 1 (answered beats hit_cap)", winner)
	}
}

func TestSelectBestOf_StructuralTiebreakPrefersEarlier(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(2, agent.Answered, true),
		makeAttempt(1, agent.Answered, true), // earlier Index
	}
	patches := []string{"p1", "p2"}
	sel := &stubSelector{picks: []string{"A", "A"}} // inconsistent

	winner, _ := selectBestOf(context.Background(), attempts, patches, "test task", sel, "m")
	if winner != 1 {
		t.Errorf("winner = %d, want 1 (structural tie -> earlier index)", winner)
	}
}

func TestSelectBestOf_ThreeAttempts(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.Answered, true),
		makeAttempt(2, agent.Answered, true),
		makeAttempt(3, agent.Answered, true),
	}
	patches := []string{"p1", "p2", "p3"}
	// 4 calls: 1v2(A=patch1,B=patch2)->"B"(patch2), 2v1(A=patch2,B=patch1)->"A"(patch2) = consistent for idx1
	// then: 2v3(A=patch2,B=patch3)->"B"(patch3), 3v2(A=patch3,B=patch2)->"A"(patch3) = consistent for idx2
	sel := &stubSelector{
		picks:   []string{"B", "A", "B", "A"},
		reasons: []string{"p2 wins", "p2", "p3 wins", "p3"},
	}

	winner, report := selectBestOf(context.Background(), attempts, patches, "test task", sel, "m")
	if winner != 2 {
		t.Errorf("winner = %d, want 2", winner)
	}
	if len(report.Verdicts) != 2 {
		t.Fatalf("verdicts = %d, want 2", len(report.Verdicts))
	}
	if sel.call != 4 {
		t.Errorf("selector called %d times, want 4", sel.call)
	}
	if report.Usage.TotalTokens != 60 {
		t.Errorf("total usage = %d, want 60", report.Usage.TotalTokens)
	}
}

func TestSelectBestOf_SelectorError(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.Answered, true),
		makeAttempt(2, agent.HitCap, true),
	}
	patches := []string{"p1", "p2"}
	sel := &stubSelector{
		picks: []string{"", ""},
		errs:  []error{errors.New("selector down"), nil},
	}

	winner, report := selectBestOf(context.Background(), attempts, patches, "test task", sel, "m")
	// Structural: answered beats hit_cap -> attempt 1 (index 0) wins.
	if winner != 0 {
		t.Errorf("winner = %d, want 0 (answered beats hit_cap)", winner)
	}
	vd := report.Verdicts[0]
	if vd.Consistent {
		t.Error("verdict should be inconsistent on error")
	}
	if !strings.Contains(vd.Reason, "selector error") {
		t.Errorf("reason should mention selector error: %q", vd.Reason)
	}
}

func TestAttemptStructuralRank(t *testing.T) {
	cases := []struct {
		name string
		a    attemptRecord
		want int
	}{
		{"answered", makeAttempt(1, agent.Answered, true), 0},
		{"hit_cap", makeAttempt(1, agent.HitCap, true), 1},
		{"killed_spiral", makeAttempt(1, agent.KilledSpiral, true), 2},
		{"killed_repeat", makeAttempt(1, agent.KilledRepeat, true), 2},
		{"provider_error", makeAttempt(1, agent.ProviderErr, true), 2},
		{"unverified", makeAttempt(1, agent.Unverified, true), 2},
		{"no_patch answered", makeAttempt(1, agent.Answered, false), 3},
		{"no_patch hit_cap", makeAttempt(1, agent.HitCap, false), 3},
	}
	for _, c := range cases {
		if got := attemptStructuralRank(c.a); got != c.want {
			t.Errorf("%s: rank = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestSelectBestByStructureAnsweredBeatsHitCap(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(1, agent.Answered, true),
		makeAttempt(2, agent.HitCap, true),
	}
	if got := selectBestByStructure(attempts); got != 0 {
		t.Errorf("got %d, want 0", got)
	}

	// Reverse order.
	attempts = []attemptRecord{
		makeAttempt(1, agent.HitCap, true),
		makeAttempt(2, agent.Answered, true),
	}
	if got := selectBestByStructure(attempts); got != 1 {
		t.Errorf("reversed: got %d, want 1", got)
	}
}

func TestSelectBestByStructureTiebreakPrefersEarlier(t *testing.T) {
	attempts := []attemptRecord{
		makeAttempt(2, agent.Answered, true),
		makeAttempt(1, agent.Answered, true), // earlier Index
	}
	if got := selectBestByStructure(attempts); got != 1 {
		t.Errorf("got %d, want 1 (earlier index)", got)
	}
}

func TestSelectBestByStructureEmptyReturnsMinusOne(t *testing.T) {
	if got := selectBestByStructure(nil); got != -1 {
		t.Errorf("got %d, want -1", got)
	}
}

func TestValidateBestOfSetup(t *testing.T) {
	cases := []struct {
		name        string
		n           int
		selectModel string
		reviewModel string
		inGit       bool
		wantErr     bool
	}{
		{"off no selector outside git", 1, "", "", false, false},
		{"zero invalid", 0, "selector", "", true, true},
		{"missing selector and review", 2, "", "", true, true},
		{"review fallback accepted", 2, "", "reviewer", true, false},
		{"select model accepted", 2, "selector", "", true, false},
		{"git required", 2, "selector", "", false, true},
	}
	for _, c := range cases {
		err := validateBestOfSetup(c.n, c.selectModel, c.reviewModel, c.inGit)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

type countingReviewer struct {
	findings []agent.ReviewFinding
	calls    int
	diffs    []string
}

func (r *countingReviewer) Review(_ context.Context, in agent.ReviewInput) (*agent.ReviewVerdict, error) {
	r.calls++
	r.diffs = append(r.diffs, in.Diff)
	return &agent.ReviewVerdict{Findings: r.findings, Model: "fake-reviewer"}, nil
}

type scriptedBestOfReviewer struct {
	verdicts []*agent.ReviewVerdict
	inputs   []agent.ReviewInput
}

func (r *scriptedBestOfReviewer) Review(_ context.Context, in agent.ReviewInput) (*agent.ReviewVerdict, error) {
	r.inputs = append(r.inputs, in)
	idx := len(r.inputs) - 1
	if idx >= len(r.verdicts) {
		return &agent.ReviewVerdict{Model: "fake-reviewer"}, nil
	}
	return r.verdicts[idx], nil
}

func initGitRepoForBestOfReviewTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	return dir
}

func TestBestOfAttemptOptionsStripsReviewer(t *testing.T) {
	reviewer := &countingReviewer{}
	opts := bestOfCLIOptions{Reviewer: reviewer, Review: true, ReviewAction: "keep", Approve: "policy"}
	attempt := bestOfAttemptOptions(opts)
	if attempt.Reviewer != nil {
		t.Fatalf("attempt Reviewer = %#v, want nil", attempt.Reviewer)
	}
	if !attempt.Review || attempt.ReviewAction != opts.ReviewAction || attempt.Approve != opts.Approve {
		t.Fatalf("attempt options changed unrelated review UI fields: %#v", attempt)
	}
	if opts.Reviewer == nil {
		t.Fatalf("bestOfAttemptOptions mutated the original options")
	}
}

func TestBestOfWinnerReviewInvokedExactlyOnce(t *testing.T) {
	dir := initGitRepoForBestOfReviewTest(t)
	baseTree, err := worktreeHeadTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("base\nfixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	reviewer := &countingReviewer{}
	base := &agent.RunResult{Task: "change file", Root: dir, Outcome: agent.Answered, Answer: "done"}
	reviewOnceCfg := agent.Config{
		Task:         "change file",
		Root:         dir,
		Sandbox:      sb,
		Reviewer:     reviewer,
		ReviewRounds: 1,
	}
	spec, rrt, rcontent, serr := reviewOnceCfg.Split()
	if serr != nil {
		t.Fatalf("resolve run spec: %v", serr)
	}
	res, err := agent.ReviewAndRepairExistingWorkspace(context.Background(), spec, rrt, rcontent, base, agent.ReviewPassOptions{BaseTree: baseTree}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.calls != 1 {
		t.Fatalf("review calls = %d, want 1", reviewer.calls)
	}
	if len(reviewer.diffs) != 1 || !strings.Contains(reviewer.diffs[0], "fixed") {
		t.Fatalf("review diff = %#v, want winner patch against pre-solve tree", reviewer.diffs)
	}
	if res.Outcome != agent.Answered {
		t.Fatalf("outcome = %s (%s), want answered", res.Outcome, res.Reason)
	}
	if res.Review == nil || res.Review.Rounds != 1 {
		t.Fatalf("review report = %#v, want one round", res.Review)
	}
}

func TestBestOfBlockedWinnerReviewYieldsUnverified(t *testing.T) {
	dir := initGitRepoForBestOfReviewTest(t)
	baseTree, err := worktreeHeadTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("base\nbroken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	reviewer := &countingReviewer{findings: []agent.ReviewFinding{{
		File:            "file.txt",
		Quote:           "broken",
		Severity:        "blocker",
		Confidence:      10,
		FailureScenario: "the selected winner leaves broken behavior",
	}}}
	base := &agent.RunResult{Task: "change file", Root: dir, Outcome: agent.Answered, Answer: "done"}
	reviewOnceCfg := agent.Config{
		Task:         "change file",
		Root:         dir,
		Sandbox:      sb,
		Reviewer:     reviewer,
		ReviewRounds: 1,
	}
	spec, rrt, rcontent, serr := reviewOnceCfg.Split()
	if serr != nil {
		t.Fatalf("resolve run spec: %v", serr)
	}
	res, err := agent.ReviewAndRepairExistingWorkspace(context.Background(), spec, rrt, rcontent, base, agent.ReviewPassOptions{BaseTree: baseTree}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.calls != 1 {
		t.Fatalf("review calls = %d, want 1", reviewer.calls)
	}
	if res.Outcome != agent.Unverified {
		t.Fatalf("outcome = %s, want unverified", res.Outcome)
	}
	if exitCodeFor(res.Outcome) != 2 {
		t.Fatalf("exit code = %d, want 2", exitCodeFor(res.Outcome))
	}
	if !strings.Contains(res.Reason, "review blockers remain") {
		t.Fatalf("reason = %q, want review blockers remain", res.Reason)
	}
	if res.Answer != "done" {
		t.Fatalf("answer = %q, want preserved winner answer", res.Answer)
	}
	if res.Review == nil || !res.Review.Blocked {
		t.Fatalf("review report = %#v, want blocked report", res.Review)
	}
}

func TestBestOfWinnerRepairReReviewsOriginalBaseAndMergesReport(t *testing.T) {
	dir := initGitRepoForBestOfReviewTest(t)
	baseTree, err := worktreeHeadTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("base\nbroken\nwinner-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	reviewer := &scriptedBestOfReviewer{verdicts: []*agent.ReviewVerdict{
		{
			Findings: []agent.ReviewFinding{{
				File:            "file.txt",
				Quote:           "broken",
				Severity:        "blocker",
				Confidence:      10,
				FailureScenario: "the selected winner leaves broken behavior",
			}},
			Usage: llm.Usage{PromptTokens: 3, TotalTokens: 5},
			Model: "fake-reviewer",
		},
		{Usage: llm.Usage{PromptTokens: 7, TotalTokens: 11}, Model: "fake-reviewer"},
	}}
	base := &agent.RunResult{Task: "change file", Root: dir, Outcome: agent.Answered, Answer: "done", Usage: llm.Usage{PromptTokens: 13, TotalTokens: 13}}
	repairSawReviewer := true
	repairCfg := agent.Config{
		Task:         "change file",
		Root:         dir,
		Sandbox:      sb,
		Reviewer:     reviewer,
		ReviewRounds: 2,
	}
	spec, rrt, rcontent, serr := repairCfg.Split()
	if serr != nil {
		t.Fatalf("resolve run spec: %v", serr)
	}
	res, err := agent.ReviewAndRepairExistingWorkspace(context.Background(), spec, rrt, rcontent, base, agent.ReviewPassOptions{BaseTree: baseTree}, func(_ context.Context, _ runspec.ResolvedSpec, rt agent.Runtime, content agent.Content) (*agent.RunResult, error) {
		repairSawReviewer = rt.Reviewer != nil
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("base\nfixed\nwinner-only\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.RunResult{Task: content.Task, Root: dir, Outcome: agent.Answered, Answer: "repaired", Usage: llm.Usage{PromptTokens: 17, TotalTokens: 17}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if repairSawReviewer {
		t.Fatalf("repair loop ran with reviewer enabled")
	}
	if len(reviewer.inputs) != 2 {
		t.Fatalf("review calls = %d, want 2", len(reviewer.inputs))
	}
	if !strings.Contains(reviewer.inputs[1].Diff, "fixed") || !strings.Contains(reviewer.inputs[1].Diff, "winner-only") {
		t.Fatalf("second review diff = %q, want full final winner diff against original base", reviewer.inputs[1].Diff)
	}
	if strings.Contains(reviewer.inputs[1].Diff, "broken") {
		t.Fatalf("second review diff still contains broken code: %q", reviewer.inputs[1].Diff)
	}
	if res.Outcome != agent.Answered {
		t.Fatalf("outcome = %s (%s), want answered", res.Outcome, res.Reason)
	}
	if res.Usage.PromptTokens != 30 || res.Usage.TotalTokens != 30 {
		t.Fatalf("usage = %+v, want winner solve usage folded into repair", res.Usage)
	}
	if res.Review == nil || res.Review.Rounds != 2 || res.Review.Status != agent.ReviewClean {
		t.Fatalf("review report = %+v, want merged clean two-round report", res.Review)
	}
	if res.Review.Usage.PromptTokens != 10 || res.Review.Usage.TotalTokens != 16 {
		t.Fatalf("review usage = %+v, want both review passes", res.Review.Usage)
	}
	if len(res.Review.Findings) != 1 || res.Review.Findings[0].Fate != agent.FateRepaired {
		t.Fatalf("findings = %+v, want round-1 blocker retained as repaired", res.Review.Findings)
	}
}
