package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// capturedSetupOracle retains the input while Grade has access to the trial
// directory (which RunTrial removes after grading).
type capturedSetupOracle struct {
	in      GradeInput
	content string
	graded  bool
}

func (o *capturedSetupOracle) Grade(_ context.Context, in GradeInput) Grade {
	o.graded = true
	o.in = in
	if b, err := os.ReadFile(filepath.Join(in.Root, "setup.txt")); err == nil {
		o.content = string(b)
	}
	return Grade{Pass: true}
}

func setupCreatedCase(o Oracle, setup func(context.Context, string, sandbox.Sandbox) (SetupResult, error)) Case {
	verify := "true"
	return Case{
		Name:         "setup-created",
		Task:         "no-op",
		Fixture:      gitRepoFixture{files: map[string]string{"tracked.txt": "base\n"}},
		Oracle:       o,
		VCSWorkspace: true,
		LadderVerify: verify,
		Config:       agent.Config{MaxIterations: 2, MaxTokens: 256},
		Setup:        setup,
	}
}

func noopTrialRunner(_ context.Context, _ agent.Config, _ string) (*agent.RunResult, *TrialLadder, error) {
	return &agent.RunResult{Outcome: agent.Answered, Answer: "done"}, nil, nil
}

func TestRunTrialPassesMatchingSetupCreatedEntryToOracle(t *testing.T) {
	const setupContent = "created by setup\n"
	o := &capturedSetupOracle{}
	c := setupCreatedCase(o, func(_ context.Context, root string, _ sandbox.Sandbox) (SetupResult, error) {
		return SetupResult{}, os.WriteFile(filepath.Join(root, "setup.txt"), []byte(setupContent), 0o644)
	})

	tr := RunTrial(context.Background(), c, Model{Label: "stub", RequiresVCS: true, Run: noopTrialRunner}, 1)
	if tr.Err != "" {
		t.Fatalf("RunTrial infra error = %q", tr.Err)
	}
	if !o.graded {
		t.Fatal("oracle was not called")
	}
	if len(o.in.SetupCreated) != 1 {
		t.Fatalf("SetupCreated = %#v, want exactly setup.txt", o.in.SetupCreated)
	}
	entry, ok := o.in.SetupCreated["setup.txt"]
	if !ok || entry.Mode == "" || entry.Object == "" {
		t.Errorf("SetupCreated[setup.txt] = %#v, want non-empty mode and object", entry)
	}
	if o.content != setupContent {
		t.Errorf("file seen by oracle = %q, want setup content %q", o.content, setupContent)
	}
}

func TestRunTrialSetupCreatedEntryDoesNotFollowRunnerMutation(t *testing.T) {
	const setupContent = "created by setup\n"
	const runnerContent = "changed by runner\n"
	o := &capturedSetupOracle{}
	c := setupCreatedCase(o, func(_ context.Context, root string, _ sandbox.Sandbox) (SetupResult, error) {
		return SetupResult{}, os.WriteFile(filepath.Join(root, "setup.txt"), []byte(setupContent), 0o644)
	})
	runner := func(_ context.Context, cfg agent.Config, _ string) (*agent.RunResult, *TrialLadder, error) {
		if err := os.WriteFile(filepath.Join(cfg.Root, "setup.txt"), []byte(runnerContent), 0o644); err != nil {
			return nil, nil, err
		}
		return &agent.RunResult{Outcome: agent.Answered, Answer: "done"}, nil, nil
	}

	tr := RunTrial(context.Background(), c, Model{Label: "stub", RequiresVCS: true, Run: runner}, 1)
	if tr.Err != "" {
		t.Fatalf("RunTrial infra error = %q", tr.Err)
	}
	entry := o.in.SetupCreated["setup.txt"]
	if entry.Mode == "" || entry.Object == "" {
		t.Fatalf("SetupCreated[setup.txt] = %#v, want recorded entry", entry)
	}
	if o.content == setupContent {
		t.Errorf("file seen by oracle still has setup content %q; runner mutation was not retained", setupContent)
	}
	if o.content != runnerContent {
		t.Errorf("file seen by oracle = %q, want runner content %q", o.content, runnerContent)
	}
}

func TestRunTrialFreshVCSInitializesBeforeSetupAndCommitsAfter(t *testing.T) {
	verify := "true"
	graded := false
	c := Case{
		Name:         "fresh-vcs-setup",
		Task:         "no-op",
		Fixture:      MapFixture{"plain.txt": "base\n"},
		Oracle:       recordingOracle{graded: &graded},
		VCSWorkspace: true,
		LadderVerify: verify,
		Config:       agent.Config{MaxIterations: 2, MaxTokens: 256},
		Setup: func(_ context.Context, root string, _ sandbox.Sandbox) (SetupResult, error) {
			if out, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").CombinedOutput(); err != nil {
				return SetupResult{}, &gitSetupError{err: err, out: string(out)}
			}
			return SetupResult{}, os.WriteFile(filepath.Join(root, "setup.txt"), []byte("setup state\n"), 0o644)
		},
	}
	tr := RunTrial(context.Background(), c, Model{Label: "stub", RequiresVCS: true, Run: noopTrialRunner}, 1)
	if tr.Err != "" {
		t.Fatalf("fresh VCS Setup should not infra-fail: %q", tr.Err)
	}
	if !graded {
		t.Error("fresh VCS trial was not graded")
	}
}

type gitSetupError struct {
	err error
	out string
}

func (e *gitSetupError) Error() string { return e.err.Error() + ": " + e.out }
