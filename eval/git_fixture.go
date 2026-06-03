package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitFixture materializes a REAL repository tree at a pinned commit into the
// trial dir — the self-history ingest path (HP-11 task diversity). Where
// MapFixture holds a hand-authored toy in-code, GitFixture archives the actual
// bytes the repo had at a base commit, so a case can be "the repo as it stood
// before commit X, minus the fix" — a genuine multi-file task that exercises
// cross-file navigation and triggers the context-window faces of HP-1 that a
// <100-LOC toy never can. It is additive (it only writes files), so a HeldOut
// oracle still layers its dropped tests on top exactly as for MapFixture.
//
// The materialized tree is a hermetic build IFF its module deps resolve from the
// host cache. The one thing that does NOT survive a copy out of the live repo is
// a RELATIVE replace directive (e.g. `replace X => ../mneme`): the sibling it
// points at is not beside the temp dir. Materialize rewrites every relative
// filesystem replace to an ABSOLUTE path resolved against RepoRoot (where the
// original go.mod lived, so the relative target meant RepoRoot/../mneme), which
// keeps the local-dep build working without a per-module allowlist.
type GitFixture struct {
	RepoRoot string // path to the live git repo to archive FROM (the source of truth).
	Ref      string // commit-ish to materialize (e.g. "ba3ba65^" for "the parent of the fix").
}

// Materialize streams `git archive <ref>` from RepoRoot into dir, then fixes up
// relative replace directives so the local-dep build resolves from the temp dir.
func (g GitFixture) Materialize(dir string) error {
	if err := g.archive(dir); err != nil {
		return err
	}
	return g.absolutizeReplaces(dir)
}

// archive extracts the tracked tree at Ref into dir by piping `git archive`'s tar
// stream straight into `tar -x`. git archive emits only tracked files at that
// commit — no untracked run artifacts, no .git — so the result is exactly the
// pinned source state.
func (g GitFixture) archive(dir string) error {
	gitCmd := exec.Command("git", "-C", g.RepoRoot, "archive", "--format=tar", g.Ref)
	tarCmd := exec.Command("tar", "-x", "-C", dir)

	pipe, err := gitCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git archive pipe: %w", err)
	}
	tarCmd.Stdin = pipe
	var gitErr bytes.Buffer
	gitCmd.Stderr = &gitErr

	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("start tar: %w", err)
	}
	if err := gitCmd.Run(); err != nil {
		return fmt.Errorf("git archive %q in %q: %w (%s)", g.Ref, g.RepoRoot, err, strings.TrimSpace(gitErr.String()))
	}
	if err := tarCmd.Wait(); err != nil {
		return fmt.Errorf("tar extract: %w", err)
	}
	return nil
}

// absolutizeReplaces rewrites each relative filesystem replace in the extracted
// go.mod to an absolute path. A relative replace target was relative to the
// ORIGINAL go.mod (in RepoRoot), so it resolves against RepoRoot — not against
// the temp dir, where the sibling does not exist. Modules without a go.mod, or
// with only registry/absolute replaces, are left untouched.
func (g GitFixture) absolutizeReplaces(dir string) error {
	out, err := g.goModEdit(dir, "-json")
	if err != nil {
		// No go.mod (a non-module fixture) is not an error — nothing to fix up.
		if strings.Contains(err.Error(), "go.mod") || strings.Contains(out, "go.mod") {
			return nil
		}
		return err
	}

	var mod struct {
		Replace []struct {
			Old struct{ Path string }
			New struct {
				Path    string
				Version string
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &mod); err != nil {
		return fmt.Errorf("parse go.mod json: %w", err)
	}

	for _, r := range mod.Replace {
		// A filesystem replace has no version; a relative one is what breaks on copy.
		if r.New.Version != "" || filepath.IsAbs(r.New.Path) || !isRelative(r.New.Path) {
			continue
		}
		abs := filepath.Clean(filepath.Join(g.RepoRoot, r.New.Path))
		if _, err := g.goModEdit(dir, "-replace="+r.Old.Path+"="+abs); err != nil {
			return fmt.Errorf("rewrite replace %s => %s: %w", r.Old.Path, abs, err)
		}
	}
	return nil
}

// goModEdit runs `go mod edit <arg>` in dir and returns combined output.
func (g GitFixture) goModEdit(dir, arg string) (string, error) {
	cmd := exec.Command("go", "mod", "edit", arg)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// isRelative reports whether a replace target is a local relative path (the kind
// that does not survive a copy), i.e. it starts with "./" or "../".
func isRelative(p string) bool {
	return strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../")
}

// Describe names the pinned source state for the manifest/logs.
func (g GitFixture) Describe() string {
	return fmt.Sprintf("git archive %s @ %s", g.Ref, g.RepoRoot)
}
