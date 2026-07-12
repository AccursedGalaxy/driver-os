package headless

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// The -report file: ONE markdown document an orchestrating agent reads after
// the run instead of stitching together jq projections, git archaeology, and
// patch files (the job delegate.sh used to do wrapper-side). Sections: result
// projection, answer, this run's diff, and the concrete next steps.

// reportOpts carries what the report needs beyond the result object itself.
type reportOpts struct {
	TraceFile string
	Repo      string
	// Delta is the pre-run dirty-state snapshot for NON-worktree git runs; nil
	// when the run was isolated (patch-based diff) or snapshotting failed.
	Delta *reportDelta
}

// reportDelta snapshots the workspace's dirty state before the run so the
// report can show only THIS run's changes: one entry per dirty/untracked file
// (status line + content hash — a porcelain-only snapshot would miss the run
// further editing an already-dirty file).
type reportDelta struct {
	entries map[string]string // path -> status+hash fingerprint
}

// captureReportDelta fingerprints the current dirty state of repo.
func captureReportDelta(repo string) (*reportDelta, error) {
	entries, err := dirtyFingerprints(repo)
	if err != nil {
		return nil, err
	}
	return &reportDelta{entries: entries}, nil
}

// dirtyFingerprints maps every dirty/untracked path to a fingerprint of its
// status and content.
func dirtyFingerprints(repo string) (map[string]string, error) {
	out, err := gitOut(repo, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	entries := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		fp := line[:2]
		if data, rerr := os.ReadFile(repo + "/" + path); rerr == nil {
			fp += fmt.Sprintf(" %x", sha1.Sum(data))
		}
		entries[path] = fp
	}
	return entries, nil
}

// changedPaths returns the paths whose fingerprint is new or different vs the
// pre-run snapshot — this run's doing (plus any concurrent editor's; worktree
// mode doesn't have that ambiguity).
func (d *reportDelta) changedPaths(repo string) ([]string, error) {
	after, err := dirtyFingerprints(repo)
	if err != nil {
		return nil, err
	}
	var paths []string
	for path, fp := range after {
		if prev, ok := d.entries[path]; !ok || prev != fp {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func gitOut(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

// writeReport renders the orchestrator report to path. Best-effort by
// contract: the caller logs the returned error; a report failure never
// changes the run's exit code.
func writeReport(path string, r resultObject, o reportOpts) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# driver-agent report (exit=%d)\n\n", r.ExitCode)
	if o.TraceFile != "" {
		fmt.Fprintf(&b, "(full live trace: %s)\n\n", o.TraceFile)
	}

	b.WriteString("## result\n\n```json\n")
	b.Write(resultProjection(r))
	b.WriteString("```\n\n")

	b.WriteString("## guarantees\n\n")
	fmt.Fprintf(&b, "verification=%s review=%s cost-bound=%s isolation=%s diff=%s bundle=%s\n", r.Guarantees.Verification.Status, r.Guarantees.Review.Status, r.Guarantees.CostBound, r.Guarantees.Isolation, r.Guarantees.Diff.Status, r.BundleStatus)
	if r.BundlePath != nil {
		fmt.Fprintf(&b, "proof_bundle=%s manifest_sha256=%s\n", *r.BundlePath, valueOrEmpty(r.BundleManifestSHA256))
	}
	if r.Outcome == "answered" && r.Guarantees.Diff.WorkspaceEffect == "unchanged" {
		b.WriteString("WARNING: answered with NO workspace changes — no-op success; if this task required a change, treat as suspect.\n")
	}
	for _, d := range r.Guarantees.Degradations {
		fmt.Fprintf(&b, "- %s: %s", d.Guarantee, d.Reason)
		if d.Detail != "" {
			fmt.Fprintf(&b, " (%s)", d.Detail)
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	b.WriteString("## answer\n\n")
	if r.Answer != nil && strings.TrimSpace(*r.Answer) != "" {
		b.WriteString(strings.TrimSpace(*r.Answer))
	} else {
		b.WriteString("(none)")
	}
	b.WriteString("\n\n")

	patchPath := ""
	if r.PatchPath != nil {
		patchPath = *r.PatchPath
	}
	switch {
	case patchPath != "":
		writePatchSection(&b, r, patchPath)
	case o.Delta != nil:
		writeDeltaSection(&b, o.Repo, o.Delta)
	case r.BaselineCommit != nil:
		// Worktree run with no patch: nothing changed (clean worktrees self-remove).
		b.WriteString("## diff\n\n(no patch — the run changed nothing; clean worktrees self-remove)\n")
	default:
		b.WriteString("## diff\n\n(no diff section — not a git repo, or the pre-run snapshot failed)\n")
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func valueOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// resultProjection is the trimmed result view the report embeds — the fields
// an orchestrator acts on, without the answer (its own section) or cache
// internals. Review/plan reports ride along whole: findings + fates live
// there, never re-derived from the trace.
func resultProjection(r resultObject) []byte {
	proj := map[string]any{
		"id": r.ID, "outcome": r.Outcome, "rescued_from": r.RescuedFrom, "exit_code": r.ExitCode,
		"iterations": r.Iterations, "reason": r.Reason, "model": r.Model,
		"review_model": r.ReviewModel, "plan_model": r.PlanModel, "select_model": r.SelectModel,
		"usage":           r.Usage,
		"solver_cost_usd": r.SolverCost, "reviewer_cost_usd": r.ReviewerCost,
		"planner_cost_usd": r.PlannerCost, "selector_cost_usd": r.SelectorCost,
		"total_cost_usd": r.TotalCost, "cost_source": r.CostSource,
		"worktree_path": r.WorktreePath, "patch_path": r.PatchPath,
		"review": r.Review, "plan": r.Plan, "best_of": r.BestOf,
		"guarantees":      r.Guarantees,
		"transcript_path": r.TranscriptPath, "error": r.Error,
		"bundle_path": r.BundlePath, "bundle_manifest_sha256": r.BundleManifestSHA256, "bundle_warning": r.BundleWarning, "bundle_status": r.BundleStatus,
	}
	out, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf("{\"marshal_error\": %q}", err.Error()))
	}
	return append(out, '\n')
}

// writePatchSection embeds the banked worktree patch and the apply/cleanup/
// ledger next steps.
func writePatchSection(b *strings.Builder, r resultObject, patchPath string) {
	fmt.Fprintf(b, "## diff (harness patch: %s)\n\n```diff\n", patchPath)
	if data, err := os.ReadFile(patchPath); err == nil {
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
	} else {
		fmt.Fprintf(b, "(reading patch failed: %v)\n", err)
	}
	b.WriteString("```\n\n## next steps\n\n")
	if r.Outcome != "answered" {
		fmt.Fprintf(b, "WARNING: outcome is %s — this patch did NOT pass the verify gate.\n", r.Outcome)
		if r.Review == nil || r.Review.Rounds == 0 {
			b.WriteString("WARNING: this patch is UNREVIEWED — no review gate ran on it.\n")
		} else if r.Review.Salvage {
			b.WriteString("NOTE: an advisory salvage review ran (report-only) — read its findings in the result block before applying.\n")
		}
		b.WriteByte('\n')
	}
	b.WriteString("Review the diff above, then apply + clean up:\n\n")
	fmt.Fprintf(b, "    git apply '%s'\n", patchPath)
	if r.WorktreePath != nil && *r.WorktreePath != "" {
		fmt.Fprintf(b, "    git worktree remove --force '%s'\n", *r.WorktreePath)
	}
	if r.ID != "" {
		fmt.Fprintf(b, "\nThen record the disposition in the delegation ledger:\n\n    ledger.sh mark '%s' landed   # or: discarded\n", r.ID)
	}
}

// writeDeltaSection shows the non-worktree run's changes: delta paths vs the
// pre-run snapshot, their diff vs HEAD, and any untracked files among them.
func writeDeltaSection(b *strings.Builder, repo string, delta *reportDelta) {
	paths, err := delta.changedPaths(repo)
	if err != nil {
		fmt.Fprintf(b, "## diff\n\n(computing the post-run delta failed: %v)\n", err)
		return
	}
	b.WriteString("## files changed by this run (delta vs pre-run snapshot)\n\n")
	if len(paths) == 0 {
		b.WriteString("(none)\n")
		return
	}
	b.WriteString(strings.Join(paths, "\n"))
	b.WriteString("\n\n## diff (delta files only — untouched pre-existing dirt excluded)\n\n```diff\n")
	diffArgs := append([]string{"diff", "HEAD", "--"}, paths...)
	if out, derr := gitOut(repo, diffArgs...); derr == nil {
		b.WriteString(out)
	} else {
		fmt.Fprintf(b, "(git diff failed: %v)\n", derr)
	}
	b.WriteString("```\n\n## untracked files among the delta (read them directly if relevant)\n\n")
	untracked, uerr := gitOut(repo, "ls-files", "--others", "--exclude-standard")
	if uerr != nil {
		fmt.Fprintf(b, "(listing untracked failed: %v)\n", uerr)
		return
	}
	inDelta := map[string]bool{}
	for _, p := range paths {
		inDelta[p] = true
	}
	found := false
	for _, u := range strings.Split(strings.TrimRight(untracked, "\n"), "\n") {
		if inDelta[u] {
			b.WriteString(u + "\n")
			found = true
		}
	}
	if !found {
		b.WriteString("(none)\n")
	}
}
