package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// gitRepoFixture materializes files and makes the dir a real git repo with a
// baseline commit — mimicking SWE-bench instances that ship .git inside the
// image, so initVCSBaseline no-ops and the oracle baseline (HEAD) predates
// Setup. This is the preexisting-HEAD shape where Setup pollution is
// invisible to the trial today.
type gitRepoFixture struct {
	files map[string]string
}

func (f gitRepoFixture) Materialize(dir string) error {
	for name, content := range f.files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, out)
		}
	}
	return nil
}

func (f gitRepoFixture) Describe() string { return "git repo with preexisting baseline commit" }

// recordingOracle notes whether the trial graded at all.
type recordingOracle struct {
	graded *bool
}

func (o recordingOracle) Grade(_ context.Context, _ GradeInput) Grade {
	*o.graded = true
	return Grade{Pass: true}
}

// TestRunTrial_SetupTrackedMutationFailsInfra is the red repro for the
// oracle-validity bug: on a fixture with a preexisting git baseline (HEAD),
// a Setup that mutates a TRACKED file runs AFTER the oracle baseline is
// fixed, so its mutation ships inside the graded diff (the swebench oracle's
// modelPatch diffs the work tree against HEAD at grade time). The trial must
// refuse to grade a polluted workspace: it must fail as an infra error
// (Trial.Err non-empty, naming setup) and never invoke the oracle.
//
// At the buggy base this test FAILS: RunTrial proceeds silently, grades, and
// Trial.Err is empty.
func TestRunTrial_SetupTrackedMutationFailsInfra(t *testing.T) {
	graded := false
	verify := "true" // trivially green in-run signal for the stub runner

	c := Case{
		Name:         "setup-pollution",
		Task:         "no-op",
		Fixture:      gitRepoFixture{files: map[string]string{"tracked.txt": "pristine\n"}},
		Oracle:       recordingOracle{graded: &graded},
		VCSWorkspace: true,
		Config:       agent.Config{MaxIterations: 2, MaxTokens: 256},
		Setup: func(_ context.Context, root string, _ sandbox.Sandbox) (SetupResult, error) {
			// The pollution vector: swebench Setup runs candidate test
			// commands in-container that can mutate tracked files.
			p := filepath.Join(root, "tracked.txt")
			if err := os.WriteFile(p, []byte("polluted by setup\n"), 0o644); err != nil {
				return SetupResult{}, err
			}
			return SetupResult{LadderVerify: &verify}, nil
		},
	}

	stub := Model{
		Label:       "stub-runner",
		RequiresVCS: true,
		Run: func(_ context.Context, _ agent.Config, _ string) (*agent.RunResult, *TrialLadder, error) {
			// The "agent" does nothing; any pollution in the graded diff can
			// only have come from Setup.
			return &agent.RunResult{Outcome: agent.Answered, Answer: "no-op"}, nil, nil
		},
	}

	tr := RunTrial(context.Background(), c, stub, 1)

	if tr.Err == "" {
		t.Fatalf("RunTrial must fail as infra when Setup mutates tracked files after the oracle baseline; got Err=%q, graded=%v", tr.Err, graded)
	}
	if !strings.Contains(strings.ToLower(tr.Err), "setup") {
		t.Errorf("infra error should name setup as the polluter; got %q", tr.Err)
	}
	if graded {
		t.Errorf("a polluted workspace must never be graded — the oracle ran")
	}
}
