package agent

// Slice 1+2 tests (REVIEW-GATE-PLAN): the review gate with a FAKE injected
// Reviewer — no network. Gate order (review only after verify green),
// quote-validation drops, the confidence gate, the repair loop + round cap,
// the cap-rescue path, fence+review interaction, observer/report plumbing,
// and slice 2's executable escalation (confirmed / refuted / capped /
// fence-respecting repros).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

// fakeReviewer returns the i-th verdict per Review call (clamping to the
// last), recording every input so tests can assert what the gate fed it.
type fakeReviewer struct {
	verdicts  [][]ReviewFinding
	summaries []string
	inputs    []ReviewInput
	err       error
	runID     string // stamped on every verdict when set (per-call suffix), like a persisting reviewer would.
}

func (f *fakeReviewer) Review(_ context.Context, in ReviewInput) (*ReviewVerdict, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	i := len(f.inputs) - 1
	var fs []ReviewFinding
	if len(f.verdicts) > 0 {
		idx := i
		if idx >= len(f.verdicts) {
			idx = len(f.verdicts) - 1
		}
		fs = f.verdicts[idx]
	}
	v := &ReviewVerdict{Findings: fs, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}}
	if len(f.summaries) > 0 {
		idx := i
		if idx >= len(f.summaries) {
			idx = len(f.summaries) - 1
		}
		v.Summary = f.summaries[idx]
	}
	if f.runID != "" {
		v.RunID = fmt.Sprintf("%s-%d", f.runID, len(f.inputs))
	}
	return v, nil
}

// gitWorkspace builds a git-initialized workspace (WriteTree needs a repo, not
// commits) with the given files, returning the sandbox and its root.
func gitWorkspace(t *testing.T, files map[string]string) (sandbox.Sandbox, string) {
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
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	return sb, root
}

// blocker builds a high-confidence blocking finding quoting content that the
// test has written into the workspace.
func blocker(file, quote string) ReviewFinding {
	return ReviewFinding{File: file, Quote: quote, Severity: "blocker", Confidence: 9, FailureScenario: "the change breaks X"}
}

// editThenAnswer scripts a solver that mutates calc.go (so the diff is
// non-empty) and then finishes.
func editThenAnswer() [][]llm.ContentPart {
	return [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},
	}
}

func reviewedRun(t *testing.T, rv Reviewer, cfg Config, turns [][]llm.ContentPart) *RunResult {
	t.Helper()
	sb, root := gitWorkspace(t, map[string]string{"calc.go": "package calc\n", "calc_test.go": "package calc // test\n"})
	ns := &nativeScript{turns: turns}
	cfg.Model, cfg.Sandbox, cfg.Root, cfg.Reviewer = ns, sb, root, rv
	if cfg.Task == "" {
		cfg.Task = "fix calc"
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A clean verdict (no findings) passes the gate: Answered, report attached
// with the reviewer's usage, and the reviewer saw the real diff + task.
func TestReviewGatePassAndInputs(t *testing.T) {
	rv := &fakeReviewer{}
	res := reviewedRun(t, rv, Config{Task: "fix the calculator"}, editThenAnswer())
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered", res.Outcome, res.Reason)
	}
	if len(rv.inputs) != 1 {
		t.Fatalf("reviewer called %d times, want 1", len(rv.inputs))
	}
	in := rv.inputs[0]
	if in.Task != "fix the calculator" {
		t.Errorf("reviewer task = %q", in.Task)
	}
	if !strings.Contains(in.Diff, "+package calc // patched") || !strings.Contains(in.Diff, "calc.go") {
		t.Errorf("reviewer diff missing the solver's change:\n%s", in.Diff)
	}
	// 4. Reviewer summary flows through to observer and report
	rv.summaries = []string{"Found a blocker."}
	ro := &recordingReviewObs{Observer: nopObserver{}}
	res = reviewedRun(t, rv, Config{Obs: ro}, editThenAnswerWithBlocker())
	if res.Review == nil || len(res.Review.Summaries) != 1 || res.Review.Summaries[0] != "Found a blocker." {
		t.Fatalf("summary not in report: %+v", res.Review)
	}
	found := false
	for _, v := range ro.events {
		if strings.HasPrefix(v, "verdict:") && strings.HasSuffix(v, ":Found a blocker.") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("summary not in observer events: %v", ro.events)
	}
}

func editThenAnswerWithBlocker() [][]llm.ContentPart {
	return [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},
	}
}

// Gate order: with VerifyCmd failing, the reviewer is NEVER consulted —
// execution first, the reviewer only rules on what execution can't decide.
func TestReviewOnlyAfterVerifyGreen(t *testing.T) {
	rv := &fakeReviewer{}
	res := reviewedRun(t, rv, Config{VerifyCmd: "false"}, editThenAnswer())
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified", res.Outcome)
	}
	if len(rv.inputs) != 0 {
		t.Fatalf("reviewer consulted despite red verification (%d calls)", len(rv.inputs))
	}
}

// Fence + review interaction: a fenced-file mutation blocks BEFORE the
// reviewer ever runs.
func TestReviewNotConsultedOnFenceViolation(t *testing.T) {
	rv := &fakeReviewer{}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "run", map[string]any{"command": "echo x >> calc_test.go"})},
		{llm.Text("done")},
	}
	res := reviewedRun(t, rv, Config{TestFence: []string{"*_test.go"}}, turns)
	if res.Outcome != Unverified || !strings.Contains(res.Reason, "test fence violated") {
		t.Fatalf("outcome = %s (%s), want fence-violated unverified", res.Outcome, res.Reason)
	}
	if len(rv.inputs) != 0 {
		t.Fatal("reviewer must not run on a fence violation")
	}
}

// Anti-hallucination: a finding whose quote does not appear verbatim in the
// named post-patch file is DROPPED (fate recorded) and never blocks.
func TestReviewQuoteValidationDrops(t *testing.T) {
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{
		blocker("calc.go", "this text is nowhere in the file"),
		blocker("missing.go", "package calc"),
	}}}
	res := reviewedRun(t, rv, Config{}, editThenAnswer())
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered (both findings dropped)", res.Outcome, res.Reason)
	}
	if n := len(res.Review.Findings); n != 2 {
		t.Fatalf("findings recorded = %d, want 2", n)
	}
	for _, f := range res.Review.Findings {
		if f.Fate != FateDropped {
			t.Errorf("finding %q fate = %s, want dropped (%s)", f.File, f.Fate, f.DropWhy)
		}
	}
}

// The confidence ladder's silent band: a blocker under the ADVISORY floor is
// a note — never blocked, never fed back (no billed repair round).
func TestReviewConfidenceGate(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.Confidence = reviewAdviseConfidence - 1
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{f}}}
	res := reviewedRun(t, rv, Config{}, editThenAnswer())
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered", res.Outcome, res.Reason)
	}
	if got := res.Review.Findings[0].Fate; got != FateNote {
		t.Fatalf("fate = %s, want note", got)
	}
	if n := len(rv.inputs); n != 1 {
		t.Fatalf("reviewer called %d times, want 1 (a note spends no repair round)", n)
	}
}

// The advisory band (the 2026-07-03 confidence-7 word-wrap case): an
// unconfirmed blocker in [advise, block) is FED BACK for a repair round —
// the solver hears the finding — but its wording says it will not block.
func TestReviewAdvisoryFedBack(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.Confidence = 7
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{f}, {}}} // round 2: clean after repair.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")}, // finish #1 → advisory feedback round.
		{structuredCall("c2", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched v2\n"})},
		{llm.Text("done again")}, // finish #2 → clean review → answered.
	}
	ns := &nativeScript{turns: turns}
	sb, root := gitWorkspace(t, map[string]string{"calc.go": "package calc\n"})
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sb, Root: root, Task: "fix calc",
		Reviewer: rv, MaxIterations: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered", res.Outcome, res.Reason)
	}
	if got := res.Review.Findings[0].Fate; got != FateAdvised {
		t.Fatalf("fate = %s, want advised", got)
	}
	var fed string
	for _, req := range ns.calls {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Text(), "[review]") {
				fed = m.Text()
			}
		}
	}
	for _, want := range []string{"LIKELY defect (advisory", "will not block", "the change breaks X", "calc.go"} {
		if !strings.Contains(fed, want) {
			t.Errorf("advisory feedback %q missing %q", fed, want)
		}
	}
}

// Advisories can never flip a run to Unverified: when they persist past the
// round budget, the run still passes (they were unconfirmed hearsay).
func TestReviewAdvisoryNeverBlocks(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.Confidence = 7
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{f}, {f}}} // the advisory persists every round.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},       // round 1 → advisory feedback.
		{llm.Text("done again")}, // round 2 → advisory again, budget gone → still answered.
	}
	res := reviewedRun(t, rv, Config{MaxIterations: 8}, turns)
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered — advisories must never block", res.Outcome, res.Reason)
	}
	if res.Review.Blocked {
		t.Fatal("report Blocked = true, want false for advisory-only findings")
	}
	for i, rf := range res.Review.Findings {
		if rf.Fate != FateAdvised {
			t.Errorf("finding %d fate = %s, want advised", i, rf.Fate)
		}
	}
}

// The repair loop: round 1 blocks → the solver receives the exact feedback
// observation and keeps working; round 2 is clean → Answered, and the round-1
// finding's fate is repaired.
func TestReviewRepairLoop(t *testing.T) {
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{blocker("calc.go", "package calc // patched")},
		{}, // round 2: clean.
	}}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")}, // finish #1 → review round 1 blocks → feedback.
		{structuredCall("c2", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched v2\n"})},
		{llm.Text("done again")}, // finish #2 → review round 2 clean.
	}
	ns := &nativeScript{}
	sb, root := gitWorkspace(t, map[string]string{"calc.go": "package calc\n"})
	ns.turns = turns
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sb, Root: root, Task: "fix calc",
		Reviewer: rv, MaxIterations: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered after repair", res.Outcome, res.Reason)
	}
	if res.Review.Rounds != 2 {
		t.Fatalf("rounds = %d, want 2", res.Review.Rounds)
	}
	if got := res.Review.Findings[0].Fate; got != FateRepaired {
		t.Fatalf("round-1 blocker fate = %s, want repaired", got)
	}
	// The feedback observation the solver read is the plan's exact shape.
	var fed string
	for _, req := range ns.calls {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Text(), "[review]") {
				fed = m.Text()
			}
		}
	}
	for _, want := range []string{"[review] a reviewer found a blocking defect", "the change breaks X", "calc.go", "do not argue with the review in prose"} {
		if !strings.Contains(fed, want) {
			t.Errorf("feedback %q missing %q", fed, want)
		}
	}
}

// Rounds exhausted with blockers standing → Unverified (exit 2 class), the
// stated reason, findings with fate expired. No new outcome codes.
func TestReviewRoundCapBlocks(t *testing.T) {
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{blocker("calc.go", "package calc // patched")},
		{blocker("calc.go", "package calc // patched")},
	}}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},       // round 1 → feedback.
		{llm.Text("done again")}, // round 2 → rounds exhausted → blocked.
	}
	res := reviewedRun(t, rv, Config{MaxIterations: 8}, turns)
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified", res.Outcome)
	}
	if !strings.Contains(res.Reason, "review blockers remain after 2 round(s)") {
		t.Fatalf("reason = %q", res.Reason)
	}
	if !res.Review.Blocked {
		t.Fatal("report.Blocked must be set")
	}
	last := res.Review.Findings[len(res.Review.Findings)-1]
	if last.Fate != FateExpired {
		t.Fatalf("final blocker fate = %s, want expired", last.Fate)
	}
}

// The cap-rescue path (upgradeIfVerified) is reviewed: a run that hits the
// iteration cap with VerifyCmd green is upgraded ONLY when review passes.
func TestReviewGatesCapRescue(t *testing.T) {
	mutate := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{structuredCall("c2", "run", map[string]any{"command": "echo spin"})},
	}
	// Blockers → the rescue is denied, outcome stays hit_cap.
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{blocker("calc.go", "package calc // patched")}}}
	res := reviewedRun(t, rv, Config{VerifyCmd: "true", MaxIterations: 2}, mutate)
	if res.Outcome != HitCap {
		t.Fatalf("outcome = %s, want hit_cap (upgrade denied by review)", res.Outcome)
	}
	if !res.Review.Blocked || len(rv.inputs) != 1 {
		t.Fatalf("cap-rescue must consult the reviewer once and record the block; got %d calls, blocked=%v", len(rv.inputs), res.Review.Blocked)
	}
	// Clean verdict → the rescue proceeds as before.
	rv2 := &fakeReviewer{}
	res2 := reviewedRun(t, rv2, Config{VerifyCmd: "true", MaxIterations: 2}, mutate)
	if res2.Outcome != Answered {
		t.Fatalf("outcome = %s, want answered (clean review rescues the cap)", res2.Outcome)
	}
}

// Not a git workspace: the gate records the skip reason and NEVER blocks.
func TestReviewSkippedOutsideGit(t *testing.T) {
	sb := sbWith(t, map[string]string{"calc.go": "package calc\n"})
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{blocker("calc.go", "package calc")}}}
	ns := &nativeScript{turns: editThenAnswer()}
	root := t.TempDir() // NOT a git repo.
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Root: root, Task: "t", Reviewer: rv})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s, want answered", res.Outcome)
	}
	if len(rv.inputs) != 0 {
		t.Fatal("reviewer must not be consulted without a git workspace")
	}
	if res.Review == nil || !strings.Contains(res.Review.Skipped, "not a git workspace") {
		t.Fatalf("report = %+v, want skip reason", res.Review)
	}
}

// A reviewer INFRA failure fails open: execution already passed, so the run
// stays Answered, loudly noted and recorded.
func TestReviewerErrorFailsOpen(t *testing.T) {
	rv := &fakeReviewer{err: errors.New("provider melted")}
	res := reviewedRun(t, rv, Config{}, editThenAnswer())
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s, want answered (fail open)", res.Outcome)
	}
	if res.Review == nil || !strings.Contains(res.Review.Skipped, "provider melted") {
		t.Fatalf("report = %+v, want the failure recorded", res.Review)
	}
}

// Reviewer nil ⇒ RunResult.Review nil and behavior untouched.
func TestNilReviewerLeavesResultUntouched(t *testing.T) {
	sb := sbWith(t, map[string]string{"calc.go": "package calc\n"})
	ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t"})
	if err != nil || res.Outcome != Answered || res.Review != nil {
		t.Fatalf("res = %+v err=%v, want plain answered with nil Review", res, err)
	}
}

// ReviewObserver events are discovered by type-assertion and fired in order.
type recordingReviewObs struct {
	Observer
	events []string
}

func (r *recordingReviewObs) ReviewStart(round int) {
	r.events = append(r.events, fmt.Sprintf("start:%d", round))
}
func (r *recordingReviewObs) ReviewFinding(f ReviewFinding) {
	r.events = append(r.events, "finding:"+f.File)
}
func (r *recordingReviewObs) ReviewVerdict(blocking, round int, summary string) {
	r.events = append(r.events, fmt.Sprintf("verdict:%d:%d:%s", blocking, round, summary))
}

func TestReviewObserverEvents(t *testing.T) {
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{blocker("calc.go", "package calc // patched")}}}
	obs := &recordingReviewObs{Observer: nopObserver{}}
	res := reviewedRun(t, rv, Config{Obs: obs, ReviewRounds: 1}, editThenAnswer())
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified (1 round, blocker)", res.Outcome)
	}
	want := []string{"start:1", "finding:calc.go", "verdict:1:1:"}
	if strings.Join(obs.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", obs.events, want)
	}
}

// The report round-trips through the transcript JSON (RunRecord).
func TestReviewReportJSONRoundTrip(t *testing.T) {
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{blocker("calc.go", "package calc // patched")}}}
	res := reviewedRun(t, rv, Config{ReviewRounds: 1}, editThenAnswer())
	rec := RecordFrom(res, "test/model")
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back RunRecord
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Review == nil || back.Review.Rounds != 1 || !back.Review.Blocked ||
		len(back.Review.Findings) != 1 || back.Review.Findings[0].Fate != FateExpired ||
		back.Review.Findings[0].FailureScenario != "the change breaks X" {
		t.Fatalf("round-trip lost data: %+v", back.Review)
	}
}

// The substance signals scanned from the diff reach the reviewer.
func TestReviewInputCarriesSubstanceSignals(t *testing.T) {
	rv := &fakeReviewer{}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "helper.go", "content": "package calc\nfunc init() { /* t.Skip( marker */ }\n"})},
		{llm.Text("done")},
	}
	res := reviewedRun(t, rv, Config{}, turns)
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if len(rv.inputs) != 1 || len(rv.inputs[0].Signals) == 0 {
		t.Fatalf("signals = %+v, want the t.Skip marker surfaced", rv.inputs)
	}
}

// ---- slice 2: executable escalation ----

// A CONFIRMED repro (command fails) blocks even below the confidence gate, and
// its output travels in the repair feedback.
func TestReproConfirmedBlocksAtLowConfidence(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.Confidence = 2 // far under the gate — only the repro can make this block.
	f.ReproCmd = "echo the-leak-reproduces && false"
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{f}, {}}}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},
		{structuredCall("c2", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched v2\n"})},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	sb, root := gitWorkspace(t, map[string]string{"calc.go": "package calc\n"})
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Root: root, Task: "t", Reviewer: rv, MaxIterations: 8})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered after repairing the confirmed defect", res.Outcome, res.Reason)
	}
	first := res.Review.Findings[0]
	if !first.Confirmed || first.Fate != FateRepaired {
		t.Fatalf("finding = %+v, want confirmed+repaired", first)
	}
	var fed string
	for _, req := range ns.calls {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Text(), "[review]") {
				fed = m.Text()
			}
		}
	}
	if !strings.Contains(fed, "CONFIRMED") || !strings.Contains(fed, "the-leak-reproduces") {
		t.Fatalf("feedback %q must carry the repro evidence", fed)
	}
}

// A repro that PASSES refutes a LOW-confidence claim: the finding is
// downgraded (fate refuted) and the run is Answered.
func TestReproRefutedDowngradesLowConfidence(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.Confidence = reviewBlockConfidence - 1
	f.ReproCmd = "true"
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{f}}}
	res := reviewedRun(t, rv, Config{}, editThenAnswer())
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered (claim refuted by execution)", res.Outcome, res.Reason)
	}
	if got := res.Review.Findings[0]; got.Fate != FateRefuted || got.Confirmed {
		t.Fatalf("finding = %+v, want fate refuted", got)
	}
}

// A repro that PASSES a HIGH-confidence claim is only downgraded to ADVISORY:
// it is fed back for repair (if budget allows) but does not block.
func TestReproAdvisedDowngradesHighConfidence(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.Confidence = reviewBlockConfidence
	f.ReproCmd = "true"
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{f}, // round 1: high-confidence clean repro -> advised.
		{},  // round 2: solver "repairs" it (clean).
	}}
	// Use 2 rounds so we can see it being fed back and then passing.
	res := reviewedRun(t, rv, Config{ReviewRounds: 2}, editThenAnswer())
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered (advised finding does not block)", res.Outcome, res.Reason)
	}
	if got := res.Review.Findings[0]; got.Fate != FateAdvised || got.Confirmed {
		t.Fatalf("finding = %+v, want fate advised", got)
	}
	if res.Review.Rounds != 2 {
		t.Errorf("rounds = %d, want 2 (advised finding should trigger a repair round)", res.Review.Rounds)
	}
}

// Repro executions are capped per round: the third repro is NOT run and its
// finding falls back to the prose ladder (high confidence → still blocks).
func TestReproCapPerRound(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "count")
	mk := func() ReviewFinding {
		f := blocker("calc.go", "package calc // patched")
		f.ReproCmd = fmt.Sprintf("echo x >> %s && true", marker)
		return f
	}
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{mk(), mk(), mk()}}}
	res := reviewedRun(t, rv, Config{ReviewRounds: 1}, editThenAnswer())
	data, _ := os.ReadFile(marker)
	if got := strings.Count(string(data), "x"); got != reviewReproCap {
		t.Fatalf("repros executed = %d, want %d", got, reviewReproCap)
	}
	// Two repros passed → refuted; the third (uncapped path) blocks on confidence.
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified (third finding blocks)", res.Outcome)
	}
}

// A repro that mutates a fenced path is REJECTED (fate dropped) and its
// damage restored — the reviewer's sin must not fail the solver's fence gate.
func TestReproFenceViolationRejectedAndRestored(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.ReproCmd = "echo sabotage >> calc_test.go && false"
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{f}}}
	sb, root := gitWorkspace(t, map[string]string{"calc.go": "package calc\n", "calc_test.go": "package calc // pristine\n"})
	ns := &nativeScript{turns: editThenAnswer()}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sb, Root: root, Task: "t",
		Reviewer: rv, TestFence: []string{"*_test.go"}, ReviewRounds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered (finding rejected)", res.Outcome, res.Reason)
	}
	got := res.Review.Findings[0]
	if got.Fate != FateDropped || !strings.Contains(got.DropWhy, "fenced") {
		t.Fatalf("finding = %+v, want dropped for touching the fence", got)
	}
	data, _ := os.ReadFile(filepath.Join(root, "calc_test.go"))
	if string(data) != "package calc // pristine\n" {
		t.Fatalf("fenced file not restored: %q", data)
	}
}

func TestReproWorkspaceMutationRejectedAndRestoredWithoutFence(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.ReproCmd = "printf 'sabotage\\n' > notes.txt && false"
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{{f}}}
	sb, root := gitWorkspace(t, map[string]string{
		"calc.go":   "package calc\n",
		"notes.txt": "pristine notes\n",
	})
	ctx := context.Background()
	baseTree, err := vcs.WriteTree(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	ns := &nativeScript{turns: editThenAnswer()}
	res, err := RunNative(ctx, Config{Model: ns, Sandbox: sb, Root: root, Task: "t", Reviewer: rv, ReviewRounds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered (finding rejected)", res.Outcome, res.Reason)
	}
	got := res.Review.Findings[0]
	if got.Fate != FateDropped || !strings.Contains(got.DropWhy, "modified the workspace") || !strings.Contains(got.DropWhy, "notes.txt") {
		t.Fatalf("finding = %+v, want dropped for workspace drift", got)
	}
	data, _ := os.ReadFile(filepath.Join(root, "notes.txt"))
	if string(data) != "pristine notes\n" {
		t.Fatalf("non-fenced repro damage not restored: %q", data)
	}
	curTree, err := vcs.WriteTree(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := vcs.DiffTrees(ctx, root, baseTree, curTree)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "+package calc // patched") || strings.Contains(diff, "sabotage") || strings.Contains(diff, "notes.txt") {
		t.Fatalf("run diff should contain only the solver change, got:\n%s", diff)
	}
}

// Each reviewer sub-run's identity flows into the report, linking the solver's
// transcript to the reviewer's own persisted transcripts (one ID per round).
func TestReviewReportCarriesReviewerRunIDs(t *testing.T) {
	rv := &fakeReviewer{runID: "rev"}
	res := reviewedRun(t, rv, Config{}, editThenAnswer())
	if res.Review == nil {
		t.Fatal("no review report")
	}
	if got := res.Review.ReviewerRuns; len(got) != 1 || got[0] != "rev-1" {
		t.Fatalf("ReviewerRuns = %v, want [rev-1]", got)
	}
}

// The gate hands every round of one run the SAME non-empty SessionKey (with the
// round number), and a different run gets a different key — the handle a
// stateful reviewer needs to continue its own round-1 exploration safely.
func TestReviewRoundsShareSessionKey(t *testing.T) {
	run := func() []ReviewInput {
		rv := &fakeReviewer{verdicts: [][]ReviewFinding{
			{blocker("calc.go", "package calc // patched")},
			{}, // round 2: clean.
		}}
		turns := [][]llm.ContentPart{
			{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
			{llm.Text("done")},
			{structuredCall("c2", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched v2\n"})},
			{llm.Text("done again")},
		}
		res := reviewedRun(t, rv, Config{ReviewRounds: 2}, turns)
		if res.Outcome != Answered || len(rv.inputs) != 2 {
			t.Fatalf("outcome=%s rounds=%d, want answered/2", res.Outcome, len(rv.inputs))
		}
		return rv.inputs
	}
	a := run()
	if a[0].SessionKey == "" || a[0].SessionKey != a[1].SessionKey {
		t.Fatalf("rounds must share one non-empty key: %q vs %q", a[0].SessionKey, a[1].SessionKey)
	}
	if a[0].Round != 1 || a[1].Round != 2 {
		t.Fatalf("rounds numbered %d,%d, want 1,2", a[0].Round, a[1].Round)
	}
	b := run()
	if b[0].SessionKey == a[0].SessionKey {
		t.Fatal("two distinct runs share a session key")
	}
}

// ---- early-stop: recurring confirmed blocker ----

// A CONFIRMED blocker (repro failed) whose File+Quote recur unchanged
// round after round triggers an early stop — the solver cannot fix it.
func TestReviewRecurringConfirmedBlockerEarlyStop(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	f.ReproCmd = "echo the-bug-persists && false" // fails → confirmed.
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{f}, // round 1: confirmed blocker.
		{f}, // round 2: same confirmed blocker recurs → early stop.
	}}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},
		{structuredCall("c2", "write_file", map[string]any{"path": "helper.go", "content": "package calc\n"})},
		{llm.Text("done again")},
	}
	res := reviewedRun(t, rv, Config{ReviewRounds: 3, MaxIterations: 8}, turns)
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified (early stop)", res.Outcome)
	}
	if !strings.Contains(res.Reason, "confirmed review blocker recurred unresolved after repair round 2") {
		t.Fatalf("reason = %q, want recurrence message", res.Reason)
	}
	if !res.Review.Blocked {
		t.Fatal("report.Blocked must be true")
	}
	if res.Review.Rounds != 2 {
		t.Fatalf("rounds = %d, want 2 (early stop, not 3)", res.Review.Rounds)
	}
	for i, rf := range res.Review.Findings {
		if rf.Fate != FateExpired {
			t.Errorf("finding %d fate = %s, want expired", i, rf.Fate)
		}
	}
}

// Different findings each round must not trigger the early-stop — the
// normal feedback loop proceeds unaltered.
func TestReviewDifferentFindingsNoEarlyStop(t *testing.T) {
	f1 := blocker("calc.go", "package calc // patched")
	f2 := blocker("calc.go", "package calc // patched v2")
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{f1}, // round 1.
		{f2}, // round 2: different quote → no recurrence.
		{},   // round 3: clean.
	}}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},
		{structuredCall("c2", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched v2\n"})},
		{llm.Text("done again")},
		{structuredCall("c3", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched v3\n"})},
		{llm.Text("done yet again")},
	}
	res := reviewedRun(t, rv, Config{ReviewRounds: 3, MaxIterations: 8}, turns)
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s (%s), want answered (no early stop)", res.Outcome, res.Reason)
	}
	if res.Review.Rounds != 3 {
		t.Fatalf("rounds = %d, want 3 (full loop)", res.Review.Rounds)
	}
}

// A recurring UNconfirmed blocker (no repro, or repro passed) must NOT
// trigger the early stop — it burns the normal round budget.
func TestReviewRecurringUnconfirmedNoEarlyStop(t *testing.T) {
	f := blocker("calc.go", "package calc // patched")
	// No repro → Confirmed stays false — the finding blocks on confidence alone.
	rv := &fakeReviewer{verdicts: [][]ReviewFinding{
		{f}, // round 1: unconfirmed blocker.
		{f}, // round 2: same unconfirmed blocker recurs.
		{f}, // round 3: maxRounds exhausted → normal block.
	}}
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
		{llm.Text("done")},
		{structuredCall("c2", "write_file", map[string]any{"path": "helper.go", "content": "package calc\n"})},
		{llm.Text("done again")},
		{llm.Text("done yet again")},
	}
	res := reviewedRun(t, rv, Config{ReviewRounds: 3, MaxIterations: 8}, turns)
	if res.Outcome != Unverified {
		t.Fatalf("outcome = %s, want unverified (rounds exhausted)", res.Outcome)
	}
	if !strings.Contains(res.Reason, "review blockers remain after 3 round(s)") {
		t.Fatalf("reason = %q, want normal exhaustion message", res.Reason)
	}
	if res.Review.Rounds != 3 {
		t.Fatalf("rounds = %d, want 3 (no early stop)", res.Review.Rounds)
	}
}
