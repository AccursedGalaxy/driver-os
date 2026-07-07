package agent

// FENCING — the harness-level enforcement that certain workspace paths are
// immutable (test fence) or the only allowed targets (diff scope). Both
// directions use the same layered design:
//
//   TEST FENCE (docs/specs/REVIEW-GATE.md stage 0, slice 0): a configured glob
//   list of paths that are READ-ONLY to the solver — canonically
//   `*_test.go,testdata/**`. The probe task text only ASKED models not to
//   modify tests; research says that hole must be closed by the HARNESS,
//   not the prompt (typia incident: an agent deleted 70% of a suite and "CI
//   was green"; anti-hack prompting alone leaves 70–95% of hacks standing).
//
//   DIFF SCOPE (the inverse fence): a configured glob list of paths the
//   solver's changes MAY touch; any change outside it is a first-class
//   failure (ScopeViolation), not a pass. It covers tasks where the TEST is
//   the deliverable and PRODUCTION must be frozen. A solver asked to add a
//   guard test that reordered production code to make its own test pass —
//   reward-hacking the verify gate — motivated it.
//
//   Unlike the test fence (denylist), scope patterns are anchored at the REPO
//   ROOT: `dir/**` is a prefix, not a substring (does NOT match
//   `pkg/dir/evil.go`), and a bare glob without a slash matches only
//   root-level files (does NOT match `pkg/x.go`). See matchesScope.
//
//   Enforcement is layered for BOTH directions:
//
//    1. Tool-layer refusal: write_file/edit_file (and the append path) REFUSE
//       a fenced path / a path outside the scope with a recovery-shaped error
//       (P3) — the cheap, immediate fence.
//    2. Closing gate: for the test fence, every fenced file is snapshotted
//       (path, sha256) at run start and re-hashed at every closing gate, so a
//       mutation that went AROUND the tools (`run` with a shell redirect,
//       sed -i, git checkout of a test file) still downgrades the run. For the
//       diff scope, a git-tree snapshot pair (run-start WriteTree vs gate-time
//       WriteTree) catches any changed/added/deleted path outside the scope.
//    3. Degrade loudly / fail closed: if the run-start snapshot fails (non-git
//       workspace for the tree, unreadable workspace for the fence walk), a
//       Note is recorded and tool-layer refusal remains active. At closing gates
//       an armed but unverifiable fence/scope blocks an Answered finish (and
//       cap rescue) without fabricating a violation from an infrastructure fault
//       (P6).
//
//   When BOTH are set: the fence WINS for fenced paths (they are read-only
//   even when in scope); a refusal message names whichever mechanism refused.
//
//   Empty Config.TestFence / Config.DiffScope = off = today's behavior
//   byte-for-byte.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// fenceContentCap bounds how many bytes of a fenced file's CONTENT the snapshot
// keeps for restoration (a reviewer repro that mutates a fenced path is undone
// from this copy — see reviewState). Files over the cap are still HASHED (drift
// detection intact) but can't be auto-restored. Test files are near-universally
// small; this exists so a pathological huge fenced file can't bloat the run.
const fenceContentCap = maxFileBytes

// fencedFile is one snapshot entry: the content hash that detects drift, and
// the bytes that undo it (nil when the file was too large to keep).
type fencedFile struct {
	hash     [sha256.Size]byte
	content  []byte
	fullHash string // sha256sum of the full file for over-cap files.
	size     int64  // file size for over-cap files (fallback).
}

// fenceState is the run-scoped fence: the globs, the model-visible absolute
// root prefix (so a docker mount-alias path like "/workspace/x_test.go" is
// fenced the same as "x_test.go"), and the run-start snapshot.
type fenceState struct {
	globs []string
	alias string
	base  map[string]fencedFile
	// snapErr records a failed snapshot walk. Tool-layer refusal still applies,
	// and closing gates fail closed as unverifiable rather than silently passing or
	// fabricating a violation out of an infrastructure fault (P6).
	snapErr error
}

// newFenceState snapshots every fenced file through the sandbox. nil when the
// fence is off (empty globs) — the caller treats nil as "no fence anywhere".
func newFenceState(ctx context.Context, cfg Config) *fenceState {
	if len(cfg.TestFence) == 0 {
		return nil
	}
	f := &fenceState{globs: cfg.TestFence, alias: sandboxAlias(cfg.Sandbox)}
	f.base, f.snapErr = walkFenced(ctx, cfg.Sandbox, cfg.TestFence)
	if f.snapErr != nil && cfg.Obs != nil {
		cfg.Obs.Note("test fence: snapshot failed (" + f.snapErr.Error() + ") — closing gates will fail closed if the run tries to finish; tool-layer refusal still active")
	}
	return f
}

// sandboxAlias returns the absolute directory the model sees as the workspace
// root ("" when the backend doesn't report one). Stripping it turns an absolute
// model-authored path back into the root-relative form the globs match.
func sandboxAlias(sb sandbox.Sandbox) string {
	if wr, ok := sb.(sandbox.WorkdirReporter); ok {
		return strings.TrimSuffix(wr.Workdir(), "/")
	}
	return ""
}

// fenceRelPath normalizes a model-authored path for glob matching: slashes,
// "./" stripped, and the sandbox alias prefix (the in-container mount point or
// the local root) removed so absolute addressing can't dodge the fence.
func fenceRelPath(alias, p string) string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if alias != "" {
		if p == alias {
			p = "."
		} else if strings.HasPrefix(p, alias+"/") {
			p = p[len(alias)+1:]
		}
	}
	p = path.Clean(p)
	return strings.TrimPrefix(p, "./")
}

// matchesFence reports whether the (already normalized) relative path is fenced.
// Three pattern shapes:
//   - bare glob, no slash (`*_test.go`): matched against the BASENAME, so a test
//     file is fenced at any depth;
//   - `dir/**`: everything under a directory of that name, at any depth
//     (`testdata/**` fences testdata/x.txt and pkg/testdata/y.json);
//   - a slashed glob (`ci/build.sh`, `.github/*`): path.Match on the full path.
func matchesFence(globs []string, rel string) bool {
	p := strings.TrimPrefix(path.Clean(filepath.ToSlash(rel)), "./")
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if dir, ok := strings.CutSuffix(g, "/**"); ok {
			if strings.HasPrefix(p, dir+"/") || strings.Contains(p, "/"+dir+"/") {
				return true
			}
			continue
		}
		if !strings.Contains(g, "/") {
			if ok, _ := path.Match(g, path.Base(p)); ok {
				return true
			}
			continue
		}
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return false
}

// matchesScope reports whether the (already normalized) relative path is inside
// the diff-scope allowlist. Unlike matchesFence — whose breadth is intentional
// for a denylist — scope patterns are anchored at the REPO ROOT:
//
//   - `dir/**`: prefix match anchored at root — matches only paths that start
//     with `dir/`. DOES NOT match `pkg/dir/evil.go` (the fence matcher's
//     `strings.Contains(p, "/dir/")` fallback is deliberately absent here —
//     a scope of `internal/**` must not admit any `*/internal/*` rando).
//   - bare glob, no slash (`*.go`): matched against the full root-relative
//     path — the path must have no `/` (root-level only). `x.go` matches
//     `x.go` but NOT `pkg/x.go`.
//   - a slashed glob (`ci/build.sh`, `.github/*`): path.Match on the full
//     path, same as matchesFence.
func matchesScope(globs []string, rel string) bool {
	p := strings.TrimPrefix(path.Clean(filepath.ToSlash(rel)), "./")
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if dir, ok := strings.CutSuffix(g, "/**"); ok {
			// Anchor at root: only match paths that start with dir/
			if strings.HasPrefix(p, dir+"/") {
				return true
			}
			continue
		}
		if !strings.Contains(g, "/") {
			// Bare glob: match only root-level files (no / in the path).
			if !strings.Contains(p, "/") {
				if ok, _ := path.Match(g, p); ok {
					return true
				}
			}
			continue
		}
		// Slashed glob: full-path match.
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return false
}

// walkFenced walks the workspace THROUGH the sandbox (ListDir + bounded reads,
// so a docker backend fences what the model sees, not the host view) and
// snapshots every file matching the fence. .git is pruned — its objects are not
// workspace files and churn on their own.
func walkFenced(ctx context.Context, sb sandbox.Sandbox, globs []string) (map[string]fencedFile, error) {
	if sb == nil {
		return nil, fmt.Errorf("no sandbox to walk")
	}
	out := map[string]fencedFile{}
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := sb.ListDir(ctx, dir)
		if err != nil {
			return fmt.Errorf("list %q: %w", dir, err)
		}
		for _, e := range entries {
			p := e.Name
			if dir != "." && dir != "" {
				p = dir + "/" + e.Name
			}
			if e.IsDir {
				if e.Name == ".git" {
					continue
				}
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			if !matchesFence(globs, p) {
				continue
			}
			data, overCap, err := readBounded(ctx, sb, p)
			if err != nil {
				return fmt.Errorf("read %q: %w", p, err)
			}
			ff := fencedFile{hash: sha256.Sum256(data)}
			if !overCap {
				ff.content = data
			} else {
				// For over-cap files, compute a full-file hash to detect mutations
				// beyond the first MiB.
				res, err := sb.Exec(ctx, sandbox.Command{Path: "sha256sum", Args: []string{"--", p}})
				if err == nil && res.ExitCode == 0 {
					fields := strings.Fields(string(res.Stdout))
					if len(fields) > 0 {
						ff.fullHash = fields[0]
					} else {
						// Fallback: record size if sha256sum output is empty.
						res, err := sb.Exec(ctx, sandbox.Command{Path: "wc", Args: []string{"-c", "--", p}})
						if err == nil && res.ExitCode == 0 {
							fmt.Sscanf(string(res.Stdout), "%d", &ff.size)
						}
					}
				} else {
					// Fallback: record size if sha256sum fails. Middle mutations
					// are undetectable in this degraded mode.
					res, err := sb.Exec(ctx, sandbox.Command{Path: "wc", Args: []string{"-c", "--", p}})
					if err == nil && res.ExitCode == 0 {
						fmt.Sscanf(string(res.Stdout), "%d", &ff.size)
					}
				}
			}
			out[p] = ff
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	return out, nil
}

// drift re-walks the fence and returns the sorted paths that no longer match
// the snapshot: modified, deleted, or NEWLY CREATED fenced files (a new
// _test.go/testdata file also changes what the verification suite means, and
// the write tools refuse creating one — the gate must agree with the tools).
// When the snapshot or re-walk is unavailable, drift reports no paths; closing
// gates use driftCheck/violationCheck so an armed but unverifiable fence fails
// closed without fabricating tampering evidence.
func (f *fenceState) drift(ctx context.Context, sb sandbox.Sandbox) []string {
	changed, _ := f.driftCheck(ctx, sb)
	return changed
}

func (f *fenceState) driftCheck(ctx context.Context, sb sandbox.Sandbox) ([]string, error) {
	if f == nil {
		return nil, nil
	}
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	cur, err := walkFenced(ctx, sb, f.globs)
	if err != nil {
		return nil, err
	}
	var out []string
	for p, ff := range cur {
		base, ok := f.base[p]
		if !ok || base.hash != ff.hash || base.fullHash != ff.fullHash || base.size != ff.size {
			out = append(out, p)
		}
	}
	for p := range f.base {
		if _, ok := cur[p]; !ok {
			out = append(out, p) // deleted.
		}
	}
	sort.Strings(out)
	return out, nil
}

// violation renders drift as the closing-gate reason ("" = clean). It is the
// exact string the plan names: the run is Unverified with the files listed.
func (f *fenceState) violation(ctx context.Context, sb sandbox.Sandbox) string {
	reason, _ := f.violationCheck(ctx, sb)
	return reason
}

func (f *fenceState) violationCheck(ctx context.Context, sb sandbox.Sandbox) (string, error) {
	changed, err := f.driftCheck(ctx, sb)
	if err != nil {
		return "", err
	}
	if len(changed) == 0 {
		return "", nil
	}
	return fmt.Sprintf("test fence violated: %s — fenced paths (%s) are read-only for this run and were modified",
		strings.Join(changed, ", "), strings.Join(f.globs, ",")), nil
}

// restore rewrites the named fenced paths back to their run-start snapshot —
// the undo for a reviewer repro command that mutated the fence (slice 2). A
// path with no snapshot (newly created) is removed via the sandbox; a path
// whose content was too large to keep is left as-is and reported.
func (f *fenceState) restore(ctx context.Context, sb sandbox.Sandbox, paths []string) error {
	for _, p := range paths {
		base, ok := f.base[p]
		switch {
		case !ok: // created during the repro — remove it.
			if _, err := sb.Exec(ctx, sandbox.Command{Path: "rm", Args: []string{"-f", "--", p}}); err != nil {
				return fmt.Errorf("remove %q: %w", p, err)
			}
		case base.content == nil:
			return fmt.Errorf("cannot restore %q: file exceeded the %d-byte snapshot cap", p, fenceContentCap)
		default:
			if err := sb.WriteFile(ctx, p, base.content, 0o644); err != nil {
				return fmt.Errorf("restore %q: %w", p, err)
			}
		}
	}
	return nil
}

// fenceRefusal is the tool-layer error for a write/edit aimed at a fenced path
// (P3: name the fence and the recovery — fix the non-test code).
func fenceRefusal(p string, globs []string) error {
	return fmt.Errorf("refused: %q matches the test fence (%s) — test files are read-only for this task; make the NON-test code satisfy them instead of changing the tests", p, strings.Join(globs, ","))
}

// applyTestFence wraps the mutation tools (write_file/edit_file — the append
// path included, since it flows through the same handlers) so a fenced path is
// refused at the tool layer. It returns a NEW map (the caller's toolset is
// never mutated) and is a no-op for an empty fence, keeping the fence-off
// behavior byte-identical. Only the two known mutation tools are wrapped —
// `run`-mediated writes are the closing re-hash's job.
func applyTestFence(tools map[string]Tool, globs []string, sb sandbox.Sandbox, cfgs ...Config) map[string]Tool {
	if len(globs) == 0 || tools == nil {
		return tools
	}
	alias := sandboxAlias(sb)
	var cfg Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	allowReproNew := func(p string) bool {
		if !cfg.ReproFirst || cfg.Root == "" {
			return false
		}
		rel := cleanRel(fenceRelPath(alias, p))
		if rel == "" || strings.HasPrefix(rel, "../") {
			return false
		}
		_, err := os.Stat(filepath.Join(cfg.Root, filepath.FromSlash(rel)))
		return os.IsNotExist(err)
	}
	out := make(map[string]Tool, len(tools))
	for k, v := range tools {
		out[k] = v
	}
	for _, name := range []string{"write_file", "edit_file"} {
		t, ok := out[name]
		if !ok {
			continue
		}
		run, runJSON := t.Run, t.RunJSON
		if run != nil {
			t.Run = func(ctx context.Context, arg string) (string, error) {
				// Both text protocols carry the path as the first whitespace token.
				p := arg
				if i := strings.IndexAny(arg, " \t"); i >= 0 {
					p = arg[:i]
				}
				if matchesFence(globs, fenceRelPath(alias, p)) && !allowReproNew(p) {
					return "", fenceRefusal(p, globs)
				}
				return run(ctx, arg)
			}
		}
		if runJSON != nil {
			t.RunJSON = func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(raw, &a) == nil && a.Path != "" &&
					matchesFence(globs, fenceRelPath(alias, a.Path)) && !allowReproNew(a.Path) {
					return "", fenceRefusal(a.Path, globs)
				}
				return runJSON(ctx, raw)
			}
		}
		out[name] = t
	}
	return out
}

// substanceSignals scans a unified diff for the cheap hack signatures the
// research names (docs/specs/REVIEW-GATE.md stage 0): a newly added t.Skip( or
// //go:build ignore (excluding tests from the suite), and assertion lines
// DELETED from test files. Signals, not verdicts — they are surfaced to the
// reviewer (ReviewInput.Signals) and recorded in the trace, and never block on
// their own.
func substanceSignals(diff string) []string {
	var out []string
	file := ""
	deletedAsserts := map[string]int{}
	var order []string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line[4:], "b/"), "a/")
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			body := line[1:]
			if strings.Contains(body, "t.Skip(") || strings.Contains(body, "t.SkipNow(") {
				out = append(out, fmt.Sprintf("added a t.Skip to %s — a test may have been disabled", file))
			}
			if strings.Contains(body, "//go:build ignore") {
				out = append(out, fmt.Sprintf("added //go:build ignore to %s — a file may have been excluded from the build", file))
			}
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			if !strings.HasSuffix(file, "_test.go") {
				continue
			}
			body := line[1:]
			for _, marker := range []string{"t.Error", "t.Fatal", "assert.", "require."} {
				if strings.Contains(body, marker) {
					if deletedAsserts[file] == 0 {
						order = append(order, file)
					}
					deletedAsserts[file]++
					break
				}
			}
		}
	}
	for _, f := range order {
		out = append(out, fmt.Sprintf("deleted %d assertion line(s) from %s", deletedAsserts[f], f))
	}
	return out
}

// scopeRefusal is the tool-layer error for a write/edit aimed at a path outside
// the diff scope (P3: name the allowed globs and the recovery — choose an
// in-scope path).
func scopeRefusal(p string, globs []string) error {
	return fmt.Errorf("refused: %q is outside the diff scope (allowed: %s) — only edit files declared in this task's scope; choose an in-scope path or explain why the scope must be changed", p, strings.Join(globs, ","))
}

// applyDiffScope wraps the mutation tools (write_file/edit_file — the append
// path included, since it flows through the same handlers) so a path outside
// the configured scope is refused at the tool layer. It returns a NEW map (the
// caller's toolset is never mutated) and is a no-op for an empty scope,
// keeping the scope-off behavior byte-identical. Only the two known mutation
// tools are wrapped — `run`-mediated writes are the closing gate's job.
func applyDiffScope(tools map[string]Tool, globs []string, sb sandbox.Sandbox) map[string]Tool {
	if len(globs) == 0 || tools == nil {
		return tools
	}
	alias := sandboxAlias(sb)
	out := make(map[string]Tool, len(tools))
	for k, v := range tools {
		out[k] = v
	}
	for _, name := range []string{"write_file", "edit_file"} {
		t, ok := out[name]
		if !ok {
			continue
		}
		run, runJSON := t.Run, t.RunJSON
		if run != nil {
			t.Run = func(ctx context.Context, arg string) (string, error) {
				p := arg
				if i := strings.IndexAny(arg, " \t"); i >= 0 {
					p = arg[:i]
				}
				if !matchesScope(globs, fenceRelPath(alias, p)) {
					return "", scopeRefusal(p, globs)
				}
				return run(ctx, arg)
			}
		}
		if runJSON != nil {
			t.RunJSON = func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(raw, &a) == nil && a.Path != "" &&
					!matchesScope(globs, fenceRelPath(alias, a.Path)) {
					return "", scopeRefusal(a.Path, globs)
				}
				return runJSON(ctx, raw)
			}
		}
		out[name] = t
	}
	return out
}
