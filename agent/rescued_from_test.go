package agent

// Cap-rescue labeling (harness audit 2026-07-07, open item 4): a run that
// upgradeIfVerified rescues from a cap/kill exit must be DISTINGUISHABLE from
// a run the model finished itself. Today the original outcome survives only
// inside the prose Reason string. RunResult gains RescuedFrom — the
// pre-upgrade Outcome ("" when the run was never rescued) — the first field of
// the typed exit contract.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A cap-rescued upgrade records the original outcome in RescuedFrom.
func TestUpgradeIfVerifiedRecordsRescuedFrom(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"x.go": "package x\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		VerifyCmd: "true",
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	// Real work happened, then the run hit the cap.
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package x // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := gs.upgradeIfVerified(ctx, &RunResult{Outcome: HitCap})
	if res.Outcome != Answered {
		t.Fatalf("outcome = %q (reason %q), want Answered (green verify on a changed tree)", res.Outcome, res.Reason)
	}
	if res.RescuedFrom != HitCap {
		t.Fatalf("RescuedFrom = %q, want %q — a cap-rescued answer must carry its original outcome", res.RescuedFrom, HitCap)
	}
}

// A refused upgrade must NOT set RescuedFrom — the run was not rescued.
func TestRefusedUpgradeDoesNotSetRescuedFrom(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"x.go": "package x\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		VerifyCmd: "true",
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	// Unchanged tree — the upgrade is refused (empty-diff guard).
	res := gs.upgradeIfVerified(ctx, &RunResult{Outcome: HitCap})
	if res.Outcome != HitCap {
		t.Fatalf("outcome = %q, want HitCap kept (empty diff refuses the upgrade)", res.Outcome)
	}
	if res.RescuedFrom != "" {
		t.Fatalf("RescuedFrom = %q, want empty on a refused upgrade", res.RescuedFrom)
	}
}
