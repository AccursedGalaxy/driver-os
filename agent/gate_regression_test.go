package agent

import (
	"context"
	"strings"
	"testing"
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
