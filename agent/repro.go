package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AccursedGalaxy/driver-os/vcs"
)

type ReproReport struct {
	Status     string `json:"status"`
	Path       string `json:"path,omitempty"`
	Cmd        string `json:"cmd,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	RedExcerpt string `json:"red_excerpt,omitempty"`
	Green      *bool  `json:"green,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type reproState struct {
	report ReproReport
	sha256 string
	iter   int
}

func (g *gates) addReproTools(tools map[string]Tool) map[string]Tool {
	if !g.d.pol.ReproFirst || tools == nil {
		return tools
	}
	out := make(map[string]Tool, len(tools)+2)
	for k, v := range tools {
		out[k] = v
	}
	out["declare_repro"] = Tool{Name: "declare_repro", NativeDesc: "Declare and lock a newly-written reproduction test file plus a targeted command. The harness validates that this file fails as a test on the pristine base tree before accepting it.", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"repo-relative path to the new repro test file already written"},"cmd":{"type":"string","description":"targeted command that runs only the repro, e.g. go test -run TestX ./pkg"}},"required":["path","cmd"]}`), RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
		var a struct{ Path, Cmd string }
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		return g.declareRepro(ctx, a.Path, a.Cmd)
	}}
	out["skip_repro"] = Tool{Name: "skip_repro", NativeDesc: "Record that this task has no executable reproduction test and explain why. Use only when a real test cannot reproduce the issue.", Schema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string","description":"why no executable repro is possible"}},"required":["reason"]}`), RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
		var a struct{ Reason string }
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		return g.skipRepro(a.Reason)
	}}
	for _, name := range []string{"write_file", "edit_file"} {
		if t, ok := out[name]; ok {
			out[name] = g.lockReproTool(t)
		}
	}
	return out
}

func (g *gates) lockReproTool(t Tool) Tool {
	alias := sandboxAlias(g.d.rt.Sandbox)
	reproFenceClosed := func(p string) bool {
		if g.repro == nil || g.repro.report.Status != "validated" || len(g.d.pol.TestFence) == 0 {
			return false
		}
		return matchesFence(g.d.pol.TestFence, fenceRelPath(alias, p))
	}
	runJSON := t.RunJSON
	if runJSON != nil {
		t.RunJSON = func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(raw, &a)
			if g.reproLockedPath(a.Path) {
				return "", fmt.Errorf("repro file %q is locked after declare_repro", a.Path)
			}
			if reproFenceClosed(a.Path) {
				return "", fenceRefusal(a.Path, g.d.pol.TestFence)
			}
			return runJSON(ctx, raw)
		}
	}
	return t
}

func (g *gates) reproLockedPath(p string) bool {
	if g.repro == nil || g.repro.report.Status != "validated" {
		return false
	}
	// Strip the sandbox workdir alias like the fence does — models routinely
	// pass workdir-absolute paths, which must still hit the lock.
	rel := cleanRel(fenceRelPath(sandboxAlias(g.d.rt.Sandbox), p))
	return rel == g.repro.report.Path
}

func cleanRel(p string) string {
	p = filepath.ToSlash(filepath.Clean(strings.TrimPrefix(p, "/")))
	if p == "." {
		return ""
	}
	return p
}
func safeJoin(root, p string) (string, string, error) {
	rel := cleanRel(p)
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(p) {
		return "", "", fmt.Errorf("path escapes workspace root")
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	return rel, full, nil
}

func (g *gates) declareRepro(ctx context.Context, p, cmd string) (string, error) {
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("cmd is empty")
	}
	if g.repro != nil && g.repro.report.Status == "validated" {
		return "", fmt.Errorf("repro already locked; to replace it, this run does not support re-declaration")
	}
	rel, full, err := safeJoin(g.d.root, p)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("write the repro test file first, then declare it")
	}
	if g.runBaseTree == "" {
		return "", fmt.Errorf("repro-first needs a captured git base tree")
	}
	cur, err := vcs.WriteTree(ctx, g.d.root)
	if err != nil {
		return "", err
	}
	// Every restore below runs uncancelable and restored only flips on
	// SUCCESS: a cancel/timeout/transient-git failure mid-validation must
	// leave the defer armed, or the workspace is silently stranded on the
	// base tree with the solver's in-flight edits gone. Caveat (documented,
	// not solved here): WriteTree is gitignore-aware and RestoreTree cleans
	// ignored files, so gitignored in-flight edits do not survive this
	// round-trip — a mid-run exposure verify/require-diff don't have.
	restored := false
	restore := func(tree string) error {
		err := vcs.RestoreTree(context.WithoutCancel(ctx), g.d.root, tree)
		if err == nil && tree == cur {
			restored = true
		}
		return err
	}
	defer func() {
		if !restored {
			_ = restore(cur)
		}
	}()
	if err := restore(g.runBaseTree); err != nil {
		return "", fmt.Errorf("could not restore base tree for validation: %w", err)
	}
	if _, err := os.Stat(full); err == nil {
		_ = restore(cur)
		return "", fmt.Errorf("repro must be a NEW file, not an edit to an existing test")
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, data, 0644); err != nil {
		return "", err
	}
	out, err := runOp(context.WithoutCancel(ctx), g.d.rt.verifySandbox(), cmd, g.d.vs.Timeout)
	if rerr := restore(cur); rerr != nil {
		return "", fmt.Errorf("could not restore workspace after base run: %w", rerr)
	}
	if err != nil {
		return "", fmt.Errorf("could not run repro command: %v", err)
	}
	grade := gradeReproRed(out)
	if isRunTimeout(out) {
		return "", fmt.Errorf("repro base run was inconclusive (timed out); narrow the command:\n%s", clip(out, 4000))
	}
	if !isRunFailure(out) {
		return "", fmt.Errorf("repro PASSES on the base tree — it does not demonstrate the bug")
	}
	if grade == "build_failure" {
		return "", fmt.Errorf("repro fails to BUILD on the base tree — it must compile against the unfixed code and fail an assertion:\n%s", clip(out, 4000))
	}
	h := sha256.Sum256(data)
	hs := hex.EncodeToString(h[:])
	g.repro = &reproState{report: ReproReport{Status: "validated", Path: rel, Cmd: cmd, RedExcerpt: clip(out, 4000)}, sha256: hs}
	if grade == "unrecognized" {
		g.repro.report.Detail = "red_signature:unrecognized"
	}
	// Only add the lock to the fence snapshot when the path matches the fence
	// globs: drift() re-walks by glob, so a non-matching entry in base would
	// read back as "deleted" — a spurious violation. Tampering on non-matching
	// paths is still caught by reproFinish's own sha256 re-check.
	if g.fence != nil && matchesFence(g.d.pol.TestFence, rel) {
		if g.fence.base == nil {
			g.fence.base = map[string]fencedFile{}
		}
		g.fence.base[rel] = fencedFile{hash: h, content: data}
	}
	return "repro validated and locked. Base failure:\n" + clip(out, 4000), nil
}

func (g *gates) skipRepro(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", fmt.Errorf("reason is empty")
	}
	if g.repro != nil && g.repro.report.Status == "validated" {
		return "", fmt.Errorf("repro already locked; cannot skip")
	}
	g.repro = &reproState{report: ReproReport{Status: "skipped", SkipReason: reason}}
	return "repro skipped: " + reason, nil
}

func gradeReproRed(out string) string {
	// "--- FAIL" means a test compiled, ran, and failed — it wins outright.
	// Generic substrings ("no such file or directory", "undefined:") must not
	// count as build signatures on their own: they routinely appear INSIDE
	// real test output (a test asserting an error message) and would
	// false-reject a valid red repro. Only go's bracketed markers count.
	if strings.Contains(out, "--- FAIL") {
		return "test_failure"
	}
	s := strings.ToLower(out)
	if strings.Contains(s, "[build failed]") || strings.Contains(s, "[setup failed]") || strings.Contains(s, "[vet failed]") || strings.Contains(s, "build constraints exclude") {
		return "build_failure"
	}
	if strings.Contains(out, "FAIL\t") {
		return "test_failure"
	}
	return "unrecognized"
}

func (g *gates) reproFinish(ctx context.Context) (feedback, block string) {
	if !g.d.pol.ReproFirst {
		return "", ""
	}
	if g.repro == nil {
		return "You must declare a failing reproduction test (declare_repro) or explicitly skip_repro before answering", "repro_missing"
	}
	if g.repro.report.Status == "skipped" {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(g.d.root, filepath.FromSlash(g.repro.report.Path)))
	if err != nil {
		return "declared repro file is missing", "repro_red"
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != g.repro.sha256 {
		return "declared repro file changed after it was locked", "repro_red"
	}
	cur, err := vcs.WriteTree(ctx, g.d.root)
	if err != nil {
		return "could not snapshot before repro check", "repro_red"
	}
	out, err := runOp(context.WithoutCancel(ctx), g.d.rt.verifySandbox(), g.repro.report.Cmd, g.d.vs.Timeout)
	_ = vcs.RestoreTree(context.WithoutCancel(ctx), g.d.root, cur)
	if err != nil {
		return "could not run declared repro", "repro_red"
	}
	if isRunTimeout(out) {
		g.repro.report.Detail = "repro_timeout"
		return "", ""
	}
	green := !isRunFailure(out)
	g.repro.report.Green = &green
	if !green {
		return "your fix does not make the declared repro pass:\n" + clip(out, 4000), "repro_red"
	}
	return "", ""
}

func (g *gates) reproReport() *ReproReport {
	if !g.d.pol.ReproFirst {
		return nil
	}
	if g.repro == nil {
		return &ReproReport{Status: "missing", Detail: "repro_missing"}
	}
	r := g.repro.report
	return &r
}
