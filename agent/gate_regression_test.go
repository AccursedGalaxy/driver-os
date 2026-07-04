package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

func TestNewGateSkipVerifyBaseline(t *testing.T) {
	cfg := Config{
		Sandbox:   sbWith(t, nil),
		Root:      ".",
		Task:      "test task",
		VerifyCmd: "echo 'run' >> side-effect.txt",
	}

	// 1. Construct NewGate.
	ctx := context.Background()
	g := NewGate(ctx, cfg)
	if g == nil {
		t.Fatal("NewGate returned nil")
	}

	// Assert the command has NOT run after NewGate returns.
	exists := func(path string) bool {
		_, err := cfg.Sandbox.ReadFile(ctx, path)
		return err == nil
	}

	if exists("side-effect.txt") {
		t.Errorf("side-effect.txt exists after NewGate, want it NOT to exist (baseline should be skipped)")
	}

	// 2. Call g.Check(ctx) once.
	_ = g.Check(ctx)

	// Assert it ran exactly once.
	if !exists("side-effect.txt") {
		t.Errorf("side-effect.txt does not exist after Check, want it to exist")
	}

	// Check content to ensure it ran exactly once.
	data, err := cfg.Sandbox.ReadFile(ctx, "side-effect.txt")
	if err != nil {
		t.Fatalf("failed to read side-effect.txt: %v", err)
	}
	content := string(data)
	count := strings.Count(content, "run")
	if count != 1 {
		t.Errorf("VerifyCmd ran %d times, want 1", count)
	}

	// Also verify the internal state shows no baseline red (since it was skipped).
	if g.g.verifyBaselineRed {
		t.Errorf("verifyBaselineRed is true, want false (skipped)")
	}
}

func TestBaselinePreamble(t *testing.T) {
	t.Run("green returns empty", func(t *testing.T) {
		cfg := Config{
			Sandbox:   sbWith(t, nil),
			VerifyCmd: "true",
		}
		ctx := context.Background()
		gs := newGates(ctx, cfg, defaultRunTimeout)
		if pre := gs.baselinePreamble(); pre != "" {
			t.Errorf("baselinePreamble() = %q, want empty on green", pre)
		}
	})

	t.Run("red returns warning", func(t *testing.T) {
		cfg := Config{
			Sandbox:   sbWith(t, nil),
			VerifyCmd: "false",
		}
		ctx := context.Background()
		gs := newGates(ctx, cfg, defaultRunTimeout)
		if !gs.verifyBaselineRed {
			t.Fatal("verifyBaselineRed is false, test setup failed — 'false' should be red")
		}
		pre := gs.baselinePreamble()
		if pre == "" {
			t.Fatal("baselinePreamble() is empty, want warning on red")
		}
		if !strings.Contains(pre, "false") {
			t.Errorf("preamble does not contain verify command 'false': %s", pre)
		}
		if !strings.Contains(pre, "exit 1") {
			t.Errorf("preamble does not contain 'exit 1' output: %s", pre)
		}
		if !strings.Contains(pre, "PRE-FLIGHT VERIFY BASELINE") {
			t.Errorf("preamble missing header: %s", pre)
		}
	})

	t.Run("skipped returns empty", func(t *testing.T) {
		cfg := Config{
			Sandbox:            sbWith(t, nil),
			VerifyCmd:          "false",
			SkipVerifyBaseline: true,
		}
		ctx := context.Background()
		gs := newGates(ctx, cfg, defaultRunTimeout)
		if pre := gs.baselinePreamble(); pre != "" {
			t.Errorf("baselinePreamble() = %q, want empty when skipped", pre)
		}
	})
}

// setupGitRepo creates a temp directory with a git repo seeded with files,
// plus a local sandbox rooted in it. Returns the sandbox and the root path.
func setupGitRepo(t *testing.T, files map[string]string) (*local.Sandbox, string) {
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
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	return sb, root
}

func TestUpgradeIfVerifiedRefusesEmptyDiff(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		VerifyCmd: "true",
		Obs:       nopObserver{},
	}
	gs := newGates(context.Background(), cfg, defaultRunTimeout)
	if gs.runBaseTree == "" {
		t.Fatal("runBaseTree is empty — baseline tree was not captured")
	}
	// The tree is unchanged vs the baseline — upgrade must be refused.
	res := gs.upgradeIfVerified(context.Background(), &RunResult{Outcome: HitCap})
	if res.Outcome != HitCap {
		t.Fatalf("Outcome = %s, want HitCap (upgrade must be refused on empty diff)", res.Outcome)
	}
	if !strings.Contains(res.Reason, "refusing upgrade") {
		t.Errorf("Reason does not mention refusing upgrade: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "baseline was already green") {
		t.Errorf("Reason does not mention baseline already green: %q", res.Reason)
	}
}

func TestUpgradeIfVerifiedUpgradesWithDiff(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		VerifyCmd: "true",
		Obs:       nopObserver{},
	}
	gs := newGates(context.Background(), cfg, defaultRunTimeout)
	if gs.runBaseTree == "" {
		t.Fatal("runBaseTree is empty — baseline tree was not captured")
	}
	// Modify the tree so the diff is non-empty.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x // patched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := gs.upgradeIfVerified(context.Background(), &RunResult{Outcome: HitCap})
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %s, want Answered (upgrade should happen with non-empty diff)", res.Outcome)
	}
}

func TestUpgradeIfVerifiedDegradesWithoutBaseline(t *testing.T) {
	// No Root → runBaseTree stays empty → upgrade proceeds as today
	// (the guard degrades to historical behavior).
	cfg := Config{
		Sandbox:   sbWith(t, nil),
		VerifyCmd: "true",
		Obs:       nopObserver{},
	}
	gs := newGates(context.Background(), cfg, defaultRunTimeout)
	if gs.runBaseTree != "" {
		t.Fatal("runBaseTree should be empty when Root is empty")
	}
	res := gs.upgradeIfVerified(context.Background(), &RunResult{Outcome: HitCap})
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %s, want Answered (upgrade must happen when baseline is unavailable)", res.Outcome)
	}
}

func TestUpgradeIfVerifiedSideEffectingVerifyDoesNotUpgrade(t *testing.T) {
	// A side-effecting verify command must not fool the guard: the pre-verify
	// tree snapshot captures the solver's state BEFORE the verify runs, so
	// verify artifacts (log files, etc.) cannot make an unchanged tree appear
	// different from the run-start baseline.
	sb, root := setupGitRepo(t, map[string]string{
		"go.mod": "module x\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		VerifyCmd: `sh -c 'printf x >> verify-artifact.txt'`,
		Obs:       nopObserver{},
	}
	gs := newGates(context.Background(), cfg, defaultRunTimeout)
	if gs.runBaseTree == "" {
		t.Fatal("runBaseTree is empty — baseline tree was not captured")
	}
	res := gs.upgradeIfVerified(context.Background(), &RunResult{Outcome: HitCap})
	if res.Outcome == Answered {
		t.Fatalf("no solver changes, side-effecting verify: got Answered; want killed outcome")
	}
	if res.Outcome != HitCap {
		t.Fatalf("Outcome = %s, want HitCap", res.Outcome)
	}
}
