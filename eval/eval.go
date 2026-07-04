// Package eval is the evaluation harness for the agent loop (HP-11): it turns a
// stochastic, multi-turn agent into MEASURABLE data — a pass-rate distribution,
// a cost, a convergence curve, and an honesty number — so a change to the
// context policy, the prompt, or the toolset can be shown to help instead of
// merely felt to.
//
// It is the consumer the agent package was shaped for: Run/RunNative already
// emit a typed RunResult (Outcome + Answer + Step trace + Usage) and take a
// silent Observer, and RunResult.Root already travels with the result as "the
// fixture hook". This package supplies the four things the dogfood bake-offs
// (docs/findings/DOGFOOD.md R9/R10) proved a serious eval needs and manual dogfooding lacked:
//
//  1. An INDEPENDENT oracle — grading is a separate judgment from the agent's
//     self-reported Outcome. Models reported `answered` with the build red; "did
//     it say done" and "is it actually done" are different questions and only the
//     Oracle answers the second.
//  2. HELD-OUT cases — the grader runs inputs the model never saw, so a parser
//     that passes the handed suite but silently accepts "2+3$" is still failed.
//  3. N runs per cell — a single trial measures the dice (R9→R10 flipped the same
//     model pass↔fail), so the unit of measurement is a distribution, not a bool.
//  4. Pristine fixtures — every Trial starts from a byte-identical copy in a fresh
//     temp dir, never the live repo and never a dir a prior trial mutated.
//
// This file holds the case + fixture vocabulary; oracle.go grades, trial.go runs
// one (Case × Model) execution, and report.go aggregates and archives.
package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Case is one task the harness can run against any model: the prompt, the
// pristine starting state (Fixture), the independent grader (Oracle), and the
// agent knobs to run it under (Config — minus the per-trial fields the runner
// fills: Model, Sandbox, Root, Obs, Task). Protocol picks the loop, mirroring
// cmd/agent: "tools" (default, native function-calling) or "text". A "tools"
// case refuses to run (infra failure) on a model that lacks tool support,
// instead of falling back to the text loop, to avoid measurement confounds.
type Case struct {
	Name     string
	Task     string       // the goal handed to the agent.
	Fixture  Fixture      // the starting state, materialized fresh per trial.
	Oracle   Oracle       // grades the finished run against ground truth (not self-report).
	Protocol string       // "tools" (default) or "text".
	Config   agent.Config // knob template (MaxIterations, MaxTokens, RunTimeout, Verify*, …).

	// Sandbox, when non-nil, builds the per-trial sandbox over the freshly
	// materialized fixture dir — the seam for cases whose execution environment is
	// not the host (SWE-bench instances exec inside their official per-instance
	// Docker image, rooted at the extracted /testbed). The trial owns the returned
	// sandbox's lifetime (Close). nil => the default host-local sandbox.
	Sandbox func(ctx context.Context, dir string) (sandbox.Sandbox, error)

	// Tools, when non-nil, builds the agent's toolset from the per-trial sandbox.
	// It is the seam for replaying a case under the PRODUCTION toolset of the binary
	// it mirrors: commit-msg and issue-bot run ReadOnlyTools (no write/edit), not the
	// full DefaultTools the loop falls back to — so without this a "review" case
	// would be handed mutation tools it never has in production, and could behave
	// (and be graded) unfaithfully. nil => the loop's DefaultTools.
	Tools func(sb sandbox.Sandbox, runTimeout time.Duration) map[string]agent.Tool

	// VCSWorkspace is true when the fixture materializes a real git work tree
	// suitable for VCS-backed runners such as the escalation ladder. Pure
	// in-memory fixtures (MapFixture) must leave this false so the ladder arm
	// can reject them cleanly rather than half-working.
	VCSWorkspace bool

	// LadderVerify is the in-run command the ladder uses as the red/green signal
	// for each rung attempt. When empty, the case verifies only via the held-out
	// oracle and is NOT ladder-compatible — validateLadderCases rejects it. The
	// case constructor sets this when it can provide a meaningful per-trial
	// verify command (e.g. a suite that the agent can pass by completing the
	// task). It is independent of the CLI -verify-cmd flag, which is an optional
	// self-check for the single-model path and is not a substitute for a
	// case-declared ladder signal.
	LadderVerify string
}

// TrialRunFunc is an optional custom trial runner. When set on a Model, RunTrial
// delegates the solve to this function instead of the normal single-model
// provider path. The runner receives the per-trial agent.Config (already
// populated with sandbox, root, task, tools) and must return a RunResult plus,
// optionally, ladder metadata. nil ladder => no ladder info.
type TrialRunFunc func(context.Context, agent.Config) (*agent.RunResult, *TrialLadder, error)

// Model pairs a provider with the human-facing id used in the report. The
// provider's Name() is its registered identity ("openrouter"), not the model
// string the run actually exercised ("openai/gpt-4o-mini"), so the label is
// carried explicitly — the report compares models, and a row needs the real id.
//
// When Run is set, RunTrial delegates the solve entirely to this function,
// skipping the normal provider/protocol selection. RequiresVCS gates the
// pre-run VCS initialisation needed by runners that depend on a clean git
// work tree (e.g. the ladder).
type Model struct {
	Label       string
	Provider    llm.Provider
	Run         TrialRunFunc
	RequiresVCS bool
}

// Fixture materializes a case's pristine starting state into a fresh directory.
// Materialize is ADDITIVE (it writes files, never deletes), so the same type
// also serves HeldOut's drop-in step: dropping a held-out test into an
// already-populated run dir is just another Materialize over the same dir.
type Fixture interface {
	Materialize(dir string) error
	Describe() string
}

// MapFixture is the simplest fixture: a path→content map written verbatim into
// the target dir (parent dirs created as needed). Paths are slash-separated and
// relative to the dir root. It keeps a small fixture in-code and hermetic — no
// embed, no nested-module embed restriction (a fixture's own go.mod is just a
// "go.mod" key), no testdata build interplay. A large fixture is better served
// by a future embed-backed DirFixture; this is what the first slice ships.
type MapFixture map[string]string

// Materialize writes every entry, creating parent directories first.
func (m MapFixture) Materialize(dir string) error {
	for path, content := range m {
		target := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("fixture mkdir for %q: %w", path, err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("fixture write %q: %w", path, err)
		}
	}
	return nil
}

// Describe lists the fixture's paths in sorted order (a stable, scannable
// summary for the manifest / logs).
func (m MapFixture) Describe() string {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return strings.Join(paths, ", ")
}
