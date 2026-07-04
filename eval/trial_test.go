package eval

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// scriptProvider plays pre-scripted turns, then defaults to a tool-call-free
// "done" reply (which RunNative reads as the final answer). It is the hermetic
// stand-in for a real model so the trial pipeline is tested without network.
type scriptProvider struct {
	name  string
	turns [][]llm.ContentPart
	i     int
}

func (p *scriptProvider) Name() string                   { return p.name }
func (p *scriptProvider) Capabilities() llm.Capabilities { return llm.Capabilities{Tools: true} }
func (p *scriptProvider) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream(p.name)
}

func (p *scriptProvider) Generate(_ context.Context, _ llm.Request) (*llm.Response, error) {
	usage := llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	if p.i >= len(p.turns) {
		return &llm.Response{Content: []llm.ContentPart{llm.Text("done")}, Usage: usage}, nil
	}
	c := p.turns[p.i]
	p.i++
	return &llm.Response{Content: c, Usage: usage}, nil
}

func writeCall(id, path, content string) llm.ToolCallPart {
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return llm.ToolCallPart{ID: id, Name: "write_file", Args: args}
}

// markerCase is a hermetic case: fix the fixture by writing "DONE" to result.txt;
// the oracle greps for it (no go toolchain, no network).
func markerCase() Case {
	return Case{
		Name:     "marker",
		Task:     "write DONE to result.txt",
		Protocol: "tools",
		Fixture:  MapFixture{"input.txt": "todo"},
		Oracle:   CommandOracle{Cmd: "grep -q DONE result.txt"},
		Config:   agent.Config{MaxIterations: 4, MaxTokens: 256, RunTimeout: 5 * time.Second},
	}
}

func TestRunTrialGrades_PassWhenWorkDone(t *testing.T) {
	// Turn 1 writes the marker; the loop then defaults to a "done" answer.
	fixer := &scriptProvider{name: "fixer", turns: [][]llm.ContentPart{
		{writeCall("1", "result.txt", "DONE")},
	}}
	tr := RunTrial(context.Background(), markerCase(), Model{Label: "fixer", Provider: fixer}, 1)

	if tr.Err != "" {
		t.Fatalf("unexpected infra error: %s", tr.Err)
	}
	if tr.Outcome != agent.Answered {
		t.Errorf("Outcome = %s, want answered", tr.Outcome)
	}
	if !tr.Pass {
		t.Errorf("oracle should PASS once the marker is written; Detail=%q", tr.Detail)
	}
	if tr.FalsePositive() {
		t.Errorf("a genuine success must not count as a false positive")
	}
	if tr.Usage.TotalTokens == 0 {
		t.Errorf("usage should accumulate across turns")
	}
}

func TestRunTrialGrades_FalsePositiveWhenClaimedButUndone(t *testing.T) {
	// The model answers immediately, never doing the work: Answered + oracle fail
	// == the false-positive the closing verification gate exists to catch.
	liar := &scriptProvider{name: "liar", turns: [][]llm.ContentPart{
		{llm.Text("all done — the file is written")},
	}}
	tr := RunTrial(context.Background(), markerCase(), Model{Label: "liar", Provider: liar}, 1)

	if tr.Outcome != agent.Answered {
		t.Fatalf("Outcome = %s, want answered (the model claimed done)", tr.Outcome)
	}
	if tr.Pass {
		t.Errorf("oracle must FAIL: no marker was written")
	}
	if !tr.FalsePositive() {
		t.Errorf("answered-but-unverified must be a false positive")
	}
}

// trackedSandbox wraps the trial's sandbox so the factory test can prove the
// case-supplied sandbox was both USED for the run and CLOSED by the trial.
type trackedSandbox struct {
	sandbox.Sandbox
	closed *bool
}

func (s trackedSandbox) Close() error {
	*s.closed = true
	return s.Sandbox.Close()
}

func TestRunTrialUsesCaseSandboxFactory(t *testing.T) {
	// The factory seam (SWE-bench: trials exec inside an instance image, not the
	// host). Here the factory hands back a tracked host-local sandbox over the
	// fixture dir — the contract under test is "factory used + factory's sandbox
	// closed", not which backend it builds.
	var built, closed bool
	c := markerCase()
	c.Sandbox = func(_ context.Context, dir string) (sandbox.Sandbox, error) {
		built = true
		lsb, err := local.New(dir)
		if err != nil {
			return nil, err
		}
		return trackedSandbox{Sandbox: lsb, closed: &closed}, nil
	}

	fixer := &scriptProvider{name: "fixer", turns: [][]llm.ContentPart{
		{writeCall("1", "result.txt", "DONE")},
	}}
	tr := RunTrial(context.Background(), c, Model{Label: "fixer", Provider: fixer}, 1)

	if !built {
		t.Fatalf("case Sandbox factory was never called")
	}
	if !closed {
		t.Errorf("trial must Close the factory's sandbox")
	}
	if tr.Err != "" || !tr.Pass {
		t.Errorf("trial should still pass through the factory sandbox: Err=%q Detail=%q", tr.Err, tr.Detail)
	}
}

// statelessFixer is concurrency-safe: it decides from the request alone (no
// mutable counter), so one instance can serve N concurrent trials — the contract
// RunConcurrent requires of a real provider. It writes the marker, then answers
// once it sees a tool result in the conversation.
type statelessFixer struct{}

func (statelessFixer) Name() string                   { return "fixer" }
func (statelessFixer) Capabilities() llm.Capabilities { return llm.Capabilities{Tools: true} }
func (statelessFixer) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("fixer")
}
func (statelessFixer) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool { // the write already happened — finish.
			return &llm.Response{Content: []llm.ContentPart{llm.Text("done")}}, nil
		}
	}
	return &llm.Response{
		Content: []llm.ContentPart{writeCall("1", "result.txt", "DONE")},
		Usage:   llm.Usage{TotalTokens: 15},
	}, nil
}

func TestRunConcurrentCollectsAllTrials(t *testing.T) {
	rep := RunConcurrent(context.Background(), []Case{markerCase()},
		[]Model{{Label: "fixer", Provider: statelessFixer{}}}, 4, 4, nil)
	if len(rep.Cells) != 1 || rep.Cells[0].N() != 4 {
		t.Fatalf("want 1 cell of 4 trials, got %d cells", len(rep.Cells))
	}
	for i, tr := range rep.Cells[0].Trials {
		if tr.Index != i+1 {
			t.Errorf("trial %d has Index %d, want %d (slot order must be preserved)", i, tr.Index, i+1)
		}
		if !tr.Pass {
			t.Errorf("trial %d should pass; Detail=%q", i, tr.Detail)
		}
	}
}

// nopReviewer satisfies agent.Reviewer; on a non-git fixture the gate skips
// before ever calling it, but the skip must still surface as a non-nil
// RunResult.Review that the Trial carries (R4b lesson: review activity must be
// visible per trial, not inferred from outcome flips).
type nopReviewer struct{}

func (nopReviewer) Review(context.Context, agent.ReviewInput) (*agent.ReviewVerdict, error) {
	return &agent.ReviewVerdict{Model: "stub/reviewer"}, nil
}

func TestRunTrialCarriesReview(t *testing.T) {
	c := markerCase()
	c.Config.Reviewer = nopReviewer{}
	fixer := &scriptProvider{name: "fixer", turns: [][]llm.ContentPart{
		{writeCall("1", "result.txt", "DONE")},
	}}
	tr := RunTrial(context.Background(), c, Model{Label: "fixer", Provider: fixer}, 1)
	if tr.Err != "" {
		t.Fatalf("unexpected infra error: %s", tr.Err)
	}
	if tr.Review == nil {
		t.Fatal("Trial.Review = nil; a run with the gate configured must carry its ReviewReport")
	}
}

type noToolProvider struct {
	scriptProvider
	called bool
}

func (p *noToolProvider) Capabilities() llm.Capabilities { return llm.Capabilities{Tools: false} }
func (p *noToolProvider) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.called = true
	return p.scriptProvider.Generate(ctx, req)
}

func TestRunTrial_RefusesTextFallback(t *testing.T) {
	p := &noToolProvider{scriptProvider: scriptProvider{name: "no-tools"}}
	c := markerCase() // Protocol: "tools" (default)
	tr := RunTrial(context.Background(), c, Model{Label: "no-tools", Provider: p}, 1)

	if tr.Protocol != "refused" {
		t.Errorf("Protocol = %q, want refused", tr.Protocol)
	}
	if tr.Err == "" || !strings.Contains(tr.Err, "refusing text fallback") {
		t.Errorf("Err = %q, want it to contain 'refusing text fallback'", tr.Err)
	}
	if tr.Outcome != "" {
		t.Errorf("Outcome = %q, want empty (never ran)", tr.Outcome)
	}
	if p.called {
		t.Errorf("provider Generate was called despite refusal")
	}
}

func TestRunTrial_ExplicitTextRuns(t *testing.T) {
	p := &noToolProvider{scriptProvider: scriptProvider{name: "text-only"}}
	c := markerCase()
	c.Protocol = "text"
	tr := RunTrial(context.Background(), c, Model{Label: "text-only", Provider: p}, 1)

	if tr.Protocol != "text" {
		t.Errorf("Protocol = %q, want text", tr.Protocol)
	}
	if tr.Err != "" {
		t.Errorf("unexpected Err: %s", tr.Err)
	}
	if !p.called {
		t.Errorf("provider Generate was NOT called for explicit text protocol")
	}
}

// TestRunTrial_LadderSeam tests the custom runner seam: a stubbed ladder runner
// returns a pass result with escalated metadata, and RunTrial should record the
// ladder fields on the Trial without disturbing the normal oracle path.
func TestRunTrial_LadderSeam(t *testing.T) {
	c := markerCase()
	c.VCSWorkspace = true // required by ladder runner; will auto-init git in temp dir.

	// Stub model with a custom runner that simulates a ladder: 2 attempts,
	// winner at rung 2 -> escalated.
	m := Model{
		Label:       "ladder:stub",
		RequiresVCS: true,
		Run: func(ctx context.Context, acfg agent.Config, _ string) (*agent.RunResult, *TrialLadder, error) {
			// Write the marker so the oracle passes.
			if err := os.WriteFile(acfg.Root+"/result.txt", []byte("DONE"), 0644); err != nil {
				return nil, nil, err
			}
			res := &agent.RunResult{
				Outcome:    agent.Answered,
				Answer:     "done",
				Iterations: 1,
				Usage:      llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			}
			meta := &TrialLadder{
				Attempts:   2,
				WinnerRung: 2,
				Escalated:  true,
				Cost:       0.42,
				Priced:     true,
			}
			return res, meta, nil
		},
	}

	tr := RunTrial(context.Background(), c, m, 1)

	if tr.Err != "" {
		t.Fatalf("unexpected infra error: %s", tr.Err)
	}
	if tr.Ladder == nil {
		t.Fatal("Trial.Ladder is nil, want ladder metadata")
	}
	if tr.Ladder.Attempts != 2 {
		t.Errorf("Ladder.Attempts = %d, want 2", tr.Ladder.Attempts)
	}
	if tr.Ladder.WinnerRung != 2 {
		t.Errorf("Ladder.WinnerRung = %d, want 2", tr.Ladder.WinnerRung)
	}
	if !tr.Ladder.Escalated {
		t.Error("Ladder.Escalated = false, want true (winner rung > 1)")
	}
	if tr.Cost != 0.42 {
		t.Errorf("Trial.Cost = %v, want 0.42 (from ladder metadata)", tr.Cost)
	}
	if !tr.Priced {
		t.Error("Trial.Priced = false, want true (ladder metadata marked priced)")
	}
	if !tr.Pass {
		t.Errorf("oracle should still pass: Detail=%q", tr.Detail)
	}
	if tr.Protocol != "ladder" {
		t.Errorf("Protocol = %q, want ladder", tr.Protocol)
	}
}

// TestRunTrial_LadderRejectsNonVCS ensures a ladder runner gets an infra error
// when the case lacks VCSWorkspace.
func TestRunTrial_LadderRejectsNonVCS(t *testing.T) {
	c := markerCase() // VCSWorkspace defaults to false.
	m := Model{
		Label:       "ladder:stub",
		RequiresVCS: true,
		Run: func(ctx context.Context, acfg agent.Config, _ string) (*agent.RunResult, *TrialLadder, error) {
			return nil, nil, nil // never called
		},
	}
	tr := RunTrial(context.Background(), c, m, 1)

	if tr.Err == "" {
		t.Fatal("expected an error for non-VCS case with ladder runner")
	}
	if !strings.Contains(tr.Err, "does not provide a git workspace") {
		t.Errorf("error message does not mention workspace requirement: %s", tr.Err)
	}
}

// TestRunTrial_VerifyCmdPropagatesToRunner ensures the case's LadderVerify
// reaches the custom trial runner's agent.Config as VerifyCmd. The ladder needs
// this in-run red/green signal; without it every attempt looks green and
// escalation never fires (ESCALATION.md §2). The test also proves that
// LadderVerify, not the CLI -verify-cmd flag, is the authority.
func TestRunTrial_VerifyCmdPropagatesToRunner(t *testing.T) {
	c := markerCase()
	c.VCSWorkspace = true
	c.LadderVerify = "go test ./..."
	// Set Config.VerifyCmd to something different — proves LadderVerify wins.
	c.Config.VerifyCmd = "false"

	var capturedCfg agent.Config
	m := Model{
		Label:       "ladder:stub",
		RequiresVCS: true,
		Run: func(ctx context.Context, acfg agent.Config, _ string) (*agent.RunResult, *TrialLadder, error) {
			capturedCfg = acfg
			// Write the marker so the oracle passes.
			if err := os.WriteFile(acfg.Root+"/result.txt", []byte("DONE"), 0644); err != nil {
				return nil, nil, err
			}
			return &agent.RunResult{
				Outcome: agent.Answered,
				Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			}, nil, nil
		},
	}

	tr := RunTrial(context.Background(), c, m, 1)
	if tr.Err != "" {
		t.Fatalf("unexpected infra error: %s", tr.Err)
	}
	if capturedCfg.VerifyCmd != "go test ./..." {
		t.Errorf("runner received VerifyCmd = %q, want %q", capturedCfg.VerifyCmd, "go test ./...")
	}
}
