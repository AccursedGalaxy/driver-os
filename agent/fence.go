package agent

// The TEST FENCE (docs/specs/REVIEW-GATE.md stage 0, slice 0): a configured glob list of
// paths that are READ-ONLY to the solver — canonically `*_test.go,testdata/**`.
// The probe task text only ASKED models not to modify tests; research says that
// hole must be closed by the HARNESS, not the prompt (typia incident: an agent
// deleted 70% of a suite and "CI was green"; anti-hack prompting alone leaves
// 70–95% of hacks standing). Enforcement is layered:
//
//  1. Tool-layer refusal: write_file/edit_file (and the append path) REFUSE a
//     fenced path with a recovery-shaped error (P3) — the cheap, immediate fence.
//  2. Closing-gate re-hash: at run start every fenced file is snapshotted
//     (path, sha256 — content kept when small enough to restore); at every
//     closing gate the fence is re-hashed, so a mutation that went AROUND the
//     tools (`run` with a shell redirect, sed -i, git checkout of a test file)
//     still downgrades the run to Unverified with a named reason. The walk goes
//     through the SANDBOX, not the host, so a docker backend fences the same
//     files the model sees.
//
// Empty Config.TestFence = fence off = today's behavior byte-for-byte.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	hash    [sha256.Size]byte
	content []byte
}

// fenceState is the run-scoped fence: the globs, the model-visible absolute
// root prefix (so a docker mount-alias path like "/workspace/x_test.go" is
// fenced the same as "x_test.go"), and the run-start snapshot.
type fenceState struct {
	globs []string
	alias string
	base  map[string]fencedFile
	// snapErr records a failed snapshot walk. The fence then degrades LOUDLY to
	// tool-layer-only enforcement (the closing re-hash is skipped): fabricating a
	// violation out of an infrastructure fault would fail honest runs (P6), and
	// silently pretending the snapshot succeeded would fake safety.
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
		cfg.Obs.Note("test fence: snapshot failed (" + f.snapErr.Error() + ") — closing-gate re-hash disabled, tool-layer refusal still active")
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
			data, _, err := readBounded(ctx, sb, p)
			if err != nil {
				return fmt.Errorf("read %q: %w", p, err)
			}
			ff := fencedFile{hash: sha256.Sum256(data)}
			if len(data) <= fenceContentCap {
				ff.content = data
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
// A nil/failed-snapshot fence never reports drift (see snapErr).
func (f *fenceState) drift(ctx context.Context, sb sandbox.Sandbox) []string {
	if f == nil || f.snapErr != nil {
		return nil
	}
	cur, err := walkFenced(ctx, sb, f.globs)
	if err != nil {
		// An unreadable workspace at gate time is an infra fault, not evidence of
		// tampering — same P6 stance as snapErr.
		return nil
	}
	var out []string
	for p, ff := range cur {
		base, ok := f.base[p]
		if !ok || base.hash != ff.hash {
			out = append(out, p)
		}
	}
	for p := range f.base {
		if _, ok := cur[p]; !ok {
			out = append(out, p) // deleted.
		}
	}
	sort.Strings(out)
	return out
}

// violation renders drift as the closing-gate reason ("" = clean). It is the
// exact string the plan names: the run is Unverified with the files listed.
func (f *fenceState) violation(ctx context.Context, sb sandbox.Sandbox) string {
	changed := f.drift(ctx, sb)
	if len(changed) == 0 {
		return ""
	}
	return fmt.Sprintf("test fence violated: %s — fenced paths (%s) are read-only for this run and were modified",
		strings.Join(changed, ", "), strings.Join(f.globs, ","))
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
func applyTestFence(tools map[string]Tool, globs []string, sb sandbox.Sandbox) map[string]Tool {
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
				// Both text protocols carry the path as the first whitespace token.
				p := arg
				if i := strings.IndexAny(arg, " \t"); i >= 0 {
					p = arg[:i]
				}
				if matchesFence(globs, fenceRelPath(alias, p)) {
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
					matchesFence(globs, fenceRelPath(alias, a.Path)) {
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
