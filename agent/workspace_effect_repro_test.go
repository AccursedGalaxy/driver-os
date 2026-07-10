package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AccursedGalaxy/driver-os/vcs"
)

// Repro for the oldest honesty gap (backlog 2026-07-05, review-triage R3):
// `answered` + an unchanged workspace looks identical to a real coding
// success. The evidence report must state the workspace effect structurally
// (never from model self-report), and an Answered run that changed nothing
// must carry a typed no-op degradation so downstream consumers can see it.
func TestFinalizeGuaranteesReportsWorkspaceEffect(t *testing.T) {
	ctx := context.Background()
	_, root := setupGitRepo(t, map[string]string{"go.mod": "module x\n"})
	base, err := vcs.WriteTree(ctx, root)
	if err != nil {
		t.Fatal(err)
	}

	// Answered + unchanged tree → effect "unchanged" + no-op degradation.
	l := &evidenceLog{root: root, baseTree: base}
	g := finalizeGuarantees(l, Answered, nil)
	if g.Diff.WorkspaceEffect != WorkspaceUnchanged {
		t.Fatalf("Diff.WorkspaceEffect = %q, want %q", g.Diff.WorkspaceEffect, WorkspaceUnchanged)
	}
	if !hasDegradation(g, "diff", ReasonNoOpAnswer) {
		t.Fatalf("Answered+unchanged must carry a %q degradation; got %#v", ReasonNoOpAnswer, g.Degradations)
	}

	// Non-answered outcomes are not no-op ANSWERS: effect still reported,
	// but no no-op degradation.
	l = &evidenceLog{root: root, baseTree: base}
	g = finalizeGuarantees(l, Unverified, nil)
	if g.Diff.WorkspaceEffect != WorkspaceUnchanged {
		t.Fatalf("Unverified Diff.WorkspaceEffect = %q, want %q", g.Diff.WorkspaceEffect, WorkspaceUnchanged)
	}
	if hasDegradation(g, "diff", ReasonNoOpAnswer) {
		t.Fatalf("no-op degradation must fire on Answered only; got %#v", g.Degradations)
	}

	// Changed tree → effect "changed", no no-op degradation. A change is a
	// change whether or not it was later committed: the comparison is the
	// content snapshot (WriteTree incl. untracked), not `git diff`.
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l = &evidenceLog{root: root, baseTree: base}
	g = finalizeGuarantees(l, Answered, nil)
	if g.Diff.WorkspaceEffect != WorkspaceChanged {
		t.Fatalf("post-edit Diff.WorkspaceEffect = %q, want %q", g.Diff.WorkspaceEffect, WorkspaceChanged)
	}
	if hasDegradation(g, "diff", ReasonNoOpAnswer) {
		t.Fatalf("changed workspace must not carry a no-op degradation; got %#v", g.Degradations)
	}

	// No baseline → effect "unknown" (honest absence, not a guess).
	l = &evidenceLog{root: root}
	g = finalizeGuarantees(l, Answered, nil)
	if g.Diff.WorkspaceEffect != WorkspaceUnknown {
		t.Fatalf("no-baseline Diff.WorkspaceEffect = %q, want %q", g.Diff.WorkspaceEffect, WorkspaceUnknown)
	}
	if hasDegradation(g, "diff", ReasonNoOpAnswer) {
		t.Fatalf("unknown effect must not claim a no-op; got %#v", g.Degradations)
	}
}
