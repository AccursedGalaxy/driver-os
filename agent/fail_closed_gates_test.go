package agent

// Fail-closed closing gates (harness audit 2026-07-07, open item 1): the
// fence/scope tree+hash re-checks must not return CLEAN on a git/walk error.
// An induced snapshot fault must not silently erase enforcement — a run whose
// armed fence/scope cannot be re-verified at the closing gate is Unverified
// (answer path) and never cap-rescued (upgrade path). It is also never a
// fabricated violation: an infra fault is not evidence of tampering (P6), so
// ScopeViolation must not be the outcome either.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// listFaultSandbox wraps a Sandbox and fails ListDir on demand — the induced
// gate-time walk fault for the fence re-hash.
type listFaultSandbox struct {
	sandbox.Sandbox
	failList bool
}

func (f *listFaultSandbox) ListDir(ctx context.Context, path string) ([]sandbox.DirEntry, error) {
	if f.failList {
		return nil, errors.New("induced walk fault")
	}
	return f.Sandbox.ListDir(ctx, path)
}

func newListFaultSandbox(t *testing.T, root string) *listFaultSandbox {
	t.Helper()
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	return &listFaultSandbox{Sandbox: sb}
}

// A fence that snapshotted fine but cannot be re-walked at the closing gate
// must fail closed: Unverified, naming the fence — not clean, not a violation.
func TestFenceUnverifiableAtGateFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newListFaultSandbox(t, root)
	cfg := Config{
		Sandbox:   fs,
		Root:      root,
		TestFence: []string{"*_test.go"},
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	fs.failList = true
	outcome, reason, _ := gs.verifySafety(ctx)
	if outcome != Unverified {
		t.Fatalf("verifySafety outcome = %q (reason %q), want Unverified — a fence walk fault must fail closed, not pass as clean", outcome, reason)
	}
	if !strings.Contains(strings.ToLower(reason), "fence") {
		t.Fatalf("reason = %q, want it to name the unverifiable fence", reason)
	}
	if strings.Contains(reason, "violated") {
		t.Fatalf("reason = %q — an infra fault must not be reported as a violation", reason)
	}
}

// A fence whose RUN-START snapshot failed can never be re-verified: the model
// answering "done" must end Unverified, not Answered.
func TestFenceSnapshotFailureFailsClosedAtFinish(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newListFaultSandbox(t, root)
	fs.failList = true // snapshot walk fails at run start
	cfg := Config{
		Sandbox:   fs,
		Root:      root,
		TestFence: []string{"*_test.go"},
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	decision := gs.finish(ctx, finishInput{answer: "done"})
	if decision.kind != finishStop {
		t.Fatalf("finish kind = %v (outcome %s, reason %q), want finishStop — an un-re-verifiable fence must not let the run finish Answered", decision.kind, decision.outcome, decision.reason)
	}
	if decision.outcome != Unverified {
		t.Fatalf("finish outcome = %q (reason %q), want Unverified", decision.outcome, decision.reason)
	}
	if !strings.Contains(strings.ToLower(decision.reason), "fence") {
		t.Fatalf("reason = %q, want it to name the unverifiable fence", decision.reason)
	}
}

// A scope whose gate-time tree snapshot fails must fail closed as Unverified —
// NOT clean (the current hole) and NOT ScopeViolation (no fabricated
// accusation from an infra fault).
func TestScopeUnverifiableAtGateFailsClosedNotViolation(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"inside/x.go": "package inside\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		DiffScope: []string{"inside/**"},
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	// Induce the gate-time git fault: the run-start snapshot succeeded, the
	// closing-gate WriteTree cannot.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	outcome, reason, _ := gs.verifySafety(ctx)
	if outcome == ScopeViolation {
		t.Fatalf("verifySafety outcome = ScopeViolation (reason %q) — an infra fault must not be reported as a violation", reason)
	}
	if outcome != Unverified {
		t.Fatalf("verifySafety outcome = %q (reason %q), want Unverified — a scope tree fault must fail closed, not pass as clean", outcome, reason)
	}
	if !strings.Contains(strings.ToLower(reason), "scope") {
		t.Fatalf("reason = %q, want it to name the unverifiable diff scope", reason)
	}
}

// A scope whose RUN-START snapshot failed (non-git workspace) can never be
// re-verified at the gate: the answer path must end Unverified, not Answered.
// (Tool-layer refusal stays active either way; this is about the honest exit.)
func TestScopeSnapshotFailureFailsClosedAtFinish(t *testing.T) {
	root := t.TempDir() // NOT a git repo — the run-start WriteTree fails.
	if err := os.MkdirAll(filepath.Join(root, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		DiffScope: []string{"inside/**"},
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	decision := gs.finish(ctx, finishInput{answer: "done"})
	if decision.kind != finishStop {
		t.Fatalf("finish kind = %v (outcome %s, reason %q), want finishStop — an un-re-verifiable scope must not let the run finish Answered", decision.kind, decision.outcome, decision.reason)
	}
	if decision.outcome != Unverified {
		t.Fatalf("finish outcome = %q (reason %q), want Unverified", decision.outcome, decision.reason)
	}
	if !strings.Contains(strings.ToLower(decision.reason), "scope") {
		t.Fatalf("reason = %q, want it to name the unverifiable diff scope", decision.reason)
	}
}

// The cap-rescue path: a HitCap run whose armed scope cannot be re-verified at
// the gate must NOT be upgraded to Answered (and must not be converted to
// ScopeViolation or Unverified either — the model never claimed to be done).
func TestUpgradeRefusedWhenScopeUnverifiable(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"inside/x.go": "package inside\n",
	})
	cfg := Config{
		Sandbox:   sb,
		Root:      root,
		DiffScope: []string{"inside/**"},
		VerifyCmd: "true",
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	// The run did real in-scope work, then hit the cap.
	if err := os.WriteFile(filepath.Join(root, "inside", "x.go"), []byte("package inside // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Induce the gate-time git fault.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	res := gs.upgradeIfVerified(ctx, &RunResult{Outcome: HitCap})
	if res.Outcome != HitCap {
		t.Fatalf("upgradeIfVerified outcome = %q (reason %q), want HitCap kept — an unverifiable scope must block the cap-rescue upgrade", res.Outcome, res.Reason)
	}
}

// Same for the fence: a HitCap run whose fence re-walk faults at the gate must
// keep its honest HitCap outcome instead of being rescued to Answered.
func TestUpgradeRefusedWhenFenceUnverifiable(t *testing.T) {
	sb, root := setupGitRepo(t, map[string]string{
		"x.go":      "package x\n",
		"x_test.go": "package x\n",
	})
	_ = sb
	fs := newListFaultSandbox(t, root)
	cfg := Config{
		Sandbox:   fs,
		Root:      root,
		TestFence: []string{"*_test.go"},
		VerifyCmd: "true",
		Obs:       &noteSpy{},
	}
	ctx := context.Background()
	gs := mustNewGates(t, ctx, cfg, defaultRunTimeout)

	// The run did real non-test work, then hit the cap.
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package x // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs.failList = true
	res := gs.upgradeIfVerified(ctx, &RunResult{Outcome: HitCap})
	if res.Outcome != HitCap {
		t.Fatalf("upgradeIfVerified outcome = %q (reason %q), want HitCap kept — an unverifiable fence must block the cap-rescue upgrade", res.Outcome, res.Reason)
	}
}
