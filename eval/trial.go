package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

// Trial is one execution of a Case by a Model: the agent's typed self-report
// (Outcome/Answer/Iters/Usage) PLUS the oracle's independent verdict (Pass/
// Detail). Keeping both side by side is what makes the false-positive metric
// computable — a run where Outcome==Answered but Pass==false is a model that
// claimed success on a task it did not finish (the #1 bake-off failure). It is a
// flat value (not the full *RunResult) so the report JSON stays compact; the
// full Step trace is intentionally out of this slice (a follow-up can archive
// per-trial traces alongside the report).
type Trial struct {
	Case      string
	Model     string
	RunID     string        // the agent run's stable ID (RunResult.ID) — the P1 spine, so a Trial can be correlated with its persisted transcript.
	Index     int           // 1-based trial number within the cell.
	Outcome   agent.Outcome // the agent's self-reported terminal state.
	Answer    string
	Iters     int
	Usage     llm.Usage
	Pass      bool   // the ORACLE's verdict — the actual grade.
	Detail    string // the oracle's evidence.
	NoAttempt bool   // the oracle found nothing gradable (Grade.NoAttempt) — demoted by best-of selection.
	Err       string // infra failure (provider/transport), distinct from a non-pass.

	Cost   float64 // USD cost of this trial's Usage at the pinned Pricing (meaningless unless Priced).
	Priced bool    // whether the model had a Pricing entry — false => render "—", not $0.

	// Plan is the opening plan-stage report (nil when Config.Planner was unset).
	// Persisted per trial so planner activity and cost are recorded facts.
	Plan       *agent.PlanReport `json:"plan,omitempty"`
	PlanCost   float64           // USD of Plan.Usage at the PLANNER model's pinned Pricing.
	PlanPriced bool              // false when the planner model has no Pricing entry (or Plan is nil).

	// Review is the closing review-gate report (nil when Config.Reviewer was
	// unset). Persisted per trial so review activity is a recorded fact, not
	// something to infer from outcome flips and billing deltas (the R4b gap).
	Review       *agent.ReviewReport `json:"review,omitempty"`
	ReviewCost   float64             // USD of Review.Usage at the REVIEWER model's pinned Pricing.
	ReviewPriced bool                // false when the reviewer model has no Pricing entry (or Review is nil).

	// LatencyMs is the agent's wall-clock for this trial (summed model + tool time
	// across steps). Derived from Steps so it survives in report.json after the full
	// trace is dropped — lets the report separate slow runs from token-heavy ones.
	LatencyMs int64

	// Protocol is the loop actually used: "tools", "text", or "refused" (infra
	// failure when a tools case met a no-tools model).
	Protocol string `json:"protocol"`

	// Ladder holds per-trial escalation-ladder metadata when the trial was
	// executed by a ladder arm. nil for normal single-model trials — the
	// omitempty tag keeps existing report JSON stable.
	Ladder *TrialLadder `json:"ladder,omitempty"`

	// Steps is the full think->act->observe trace, kept for per-trial archival but
	// EXCLUDED from report.json (json:"-") so the aggregate report stays compact —
	// WriteFiles writes each trace to its own file under traces/ instead.
	Steps []agent.Step `json:"-"`
}

// FalsePositive reports the run that claimed done but did not actually finish:
// the agent terminated with Answered, yet the oracle failed it. This is the
// honesty signal the closing verification gate (VerifyCmd) exists to drive to
// zero — measuring it here closes the loop on that feature.
func (t Trial) FalsePositive() bool {
	return t.Outcome == agent.Answered && !t.Pass
}

// TrialLadder records the per-trial escalation-ladder metadata for a trial run
// by a ladder arm. Zero values mean "no info available" — the report renders
// these as "—".
type TrialLadder struct {
	Attempts   int     // total attempts across all rungs.
	WinnerRung int     // 1-based rung index of the winning attempt; 0 when nothing won.
	Escalated  bool    // true when any attempt went past rung 1 (the NORTH-STAR guard).
	Cost       float64 // USD cost of all ladder attempts summed; valid only when Priced is true.
	Priced     bool    // whether at least one attempt had a cost known to the price table.

	// CostByModel is the per-model dollar attribution: solver cost under its rung
	// model id, reviewer cost under the reviewer model id, planner under the
	// planner model id. The sum across entries equals Cost (modulo pricing
	// gaps). nil for non-ladder trials (omitempty keeps JSON compact).
	CostByModel map[string]float64 `json:"cost_by_model,omitempty"`
}

// RunTrial executes one trial: materialize a PRISTINE fixture into a fresh temp
// dir (never the live repo, never a reused dir), run the agent against it, then
// grade the resulting on-disk state with the case's oracle. The temp dir is
// removed afterward — the fixture is reproducible, and the verdict is what we
// keep. err-shaped infra failures from the loop are recorded on the Trial (as
// .Err) rather than returned, so one flaky provider call can't abort a whole
// suite sweep; the trial just counts as a non-pass.
//
// When m.Run is set (a custom trial runner, e.g. the escalation ladder),
// RunTrial delegates the solve to that function instead of the normal
// single-model provider path. m.RequiresVCS gates a pre-run git-baseline
// initialisation for runners that need a clean work tree.
func RunTrial(ctx context.Context, c Case, m Model, index int) Trial {
	tr := Trial{Case: c.Name, Model: m.Label, Index: index}

	// Static checks that must run BEFORE any fixture materialisation, so a
	// misconfigured pair never wastes time pulling a SWE-bench image or
	// unpacking a large archive.

	// 1. Custom-runner (ladder) VCS gate: a case that declares no git
	//    workspace can be rejected immediately.
	if m.Run != nil && m.RequiresVCS && !c.VCSWorkspace {
		tr.Err = fmt.Sprintf("ladder: case %q does not provide a git workspace (map fixtures are not ladder-compatible)", c.Name)
		return tr
	}

	// 2. Normal single-model path: protocol/capability mismatch must not
	//    materialise the fixture (the original HP-11 guard).
	var run func(context.Context, agent.Config) (*agent.RunResult, error)
	var customRun func(context.Context, agent.Config, string) (*agent.RunResult, *TrialLadder, error)

	if m.Run != nil {
		customRun = m.Run
		tr.Protocol = "ladder"
	} else {
		switch {
		case c.Protocol == "text":
			tr.Protocol = "text"
			run = agent.Run
		case m.Provider.Capabilities().Tools:
			tr.Protocol = "tools"
			run = agent.RunNative
		default:
			tr.Protocol = "refused"
			tr.Err = "protocol: case requires native tools but model lacks tool support (refusing text fallback)"
			return tr
		}
	}

	dir, err := os.MkdirTemp("", "eval-"+c.Name+"-")
	if err != nil {
		tr.Err = "mkdtemp: " + err.Error()
		return tr
	}
	defer os.RemoveAll(dir)

	if err := c.Fixture.Materialize(dir); err != nil {
		tr.Err = "fixture: " + err.Error()
		return tr
	}

	// VCS plumbing is initialized before Setup because Setup may invoke git.
	// Fresh repositories deliberately defer their first commit until Setup has
	// established the agent's true starting state.
	freshVCS := false
	if m.Run != nil && m.RequiresVCS {
		var err error
		freshVCS, err = initVCSBaseline(dir)
		if err != nil {
			tr.Err = "ladder: vcs baseline: " + err.Error()
			return tr
		}
	}

	// The case's sandbox factory, when set, replaces the default host-local
	// sandbox (e.g. SWE-bench trials exec inside the instance's Docker image).
	newSandbox := func(ctx context.Context, dir string) (sandbox.Sandbox, error) {
		return local.New(dir)
	}
	if c.Sandbox != nil {
		newSandbox = c.Sandbox
	}
	sb, err := newSandbox(ctx, dir)
	if err != nil {
		tr.Err = "sandbox: " + err.Error()
		return tr
	}
	defer sb.Close()

	var setupLadderVerify *string
	var setupCreated map[string]TreeEntry
	if customRun != nil && c.Setup != nil {
		// Existing repositories have an oracle baseline at HEAD. Snapshot the
		// complete tree before Setup without touching its real index.
		var before string
		if c.VCSWorkspace && !freshVCS {
			before, err = vcs.WriteTree(ctx, dir)
			if err != nil {
				tr.Err = "setup: snapshot before setup: " + err.Error()
				return tr
			}
		}
		res, err := c.Setup(ctx, dir, sb)
		if err != nil {
			tr.Err = "setup: " + err.Error()
			return tr
		}
		setupLadderVerify = res.LadderVerify
		if c.VCSWorkspace && !freshVCS {
			setupCreated, err = setupMutationCheck(ctx, dir, before)
			if err != nil {
				tr.Err = "setup: " + err.Error()
				return tr
			}
		}
	}
	if freshVCS {
		if err := commitVCSBaseline(dir); err != nil {
			tr.Err = "ladder: vcs baseline: " + err.Error()
			return tr
		}
	}

	cfg := c.Config // copy the knob template; fill the per-trial fields.
	cfg.Model = m.Provider
	cfg.Sandbox = sb
	cfg.Task = c.Task
	if c.TaskSteer != "" {
		cfg.Task += "\n\n" + c.TaskSteer
	}
	cfg.Root = dir
	cfg.Obs = nil // silent: a trial yields data, not stdout (that's the Observer seam).

	// When the case declares a ladder verify signal, install it on the config
	// that the custom runner (ladder) receives.  This is independent of the
	// CLI -verify-cmd flag — the case constructor is the authority on what a
	// meaningful in-run red/green signal looks like for this task.
	ladderVerify := c.LadderVerify
	if setupLadderVerify != nil {
		ladderVerify = *setupLadderVerify
	}
	if customRun != nil {
		if ladderVerify == "" {
			tr.Err = fmt.Sprintf("ladder: case %q has no validated LadderVerify signal", c.Name)
			return tr
		}
		cfg.VerifyCmd = ladderVerify
		// Auto-verify is never armed in eval; LadderVerify is the case authority.
		// Abort immediately if the verify baseline is red — a permanently-red
		// gate (e.g. test ids that don't exist on the base checkout) would burn
		// every ladder attempt to hit_cap for no gain.
		cfg.AbortOnRedBaseline = true
	}

	// Production-faithful toolset, when the case declares one — otherwise the loop
	// falls back to DefaultTools. This binds the case's tool factory to THIS trial's
	// sandbox (tools are sandbox-scoped, so the Case can only carry a factory).
	if c.Tools != nil {
		rt := cfg.RunTimeout
		if rt <= 0 {
			rt = 60 * time.Second
		}
		cfg.Tools = c.Tools(sb, rt)
	}

	var res *agent.RunResult
	var runErr error
	var ladder *TrialLadder

	if customRun != nil {
		res, ladder, runErr = customRun(ctx, cfg, c.Name)
	} else {
		res, runErr = run(ctx, cfg)
	}

	if res != nil {
		tr.RunID = res.ID
		tr.Outcome = res.Outcome
		tr.Answer = res.Answer
		tr.Iters = res.Iterations
		tr.Usage = res.Usage
		tr.Steps = res.Steps
		for _, s := range res.Steps {
			tr.LatencyMs += s.ModelMs + s.ToolMs
		}
		tr.Review = res.Review
		if res.Review != nil {
			tr.ReviewCost, tr.ReviewPriced = CostOf(res.Review.ReviewerModel, res.Review.Usage)
		}
		tr.Plan = res.Plan
		if res.Plan != nil {
			tr.PlanCost, tr.PlanPriced = CostOf(res.Plan.Model, res.Plan.Usage)
		}
	}

	if ladder != nil {
		tr.Ladder = ladder
		// Cost for ladder trials is set from the runner's summed attempt
		// costs — do NOT call CostOf(m.Label, tr.Usage) because the ladder
		// arm label has no Pricing entry and the per-attempt usage is not
		// on the top-level RunResult.
		tr.Cost = ladder.Cost
		tr.Priced = ladder.Priced
	} else {
		tr.Cost, tr.Priced = CostOf(m.Label, tr.Usage)
	}
	if runErr != nil {
		tr.Err = runErr.Error()
	}

	// Grade the post-run state regardless of how the loop ended — a provider
	// error or a hit-cap simply leaves the fixture unfinished, which the oracle
	// will fail (correctly, and without counting as a false positive since the
	// Outcome is not Answered).
	g := c.Oracle.Grade(ctx, GradeInput{Root: dir, Sandbox: sb, Result: res, SetupCreated: setupCreated})
	tr.Pass = g.Pass
	tr.NoAttempt = g.NoAttempt
	tr.Detail = g.Detail
	return tr
}

// initVCSBaseline ensures dir is a git work tree. It returns true when it
// created a fresh repository; its baseline commit is deliberately deferred
// until after Setup.
func initVCSBaseline(dir string) (bool, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	if err := cmd.Run(); err == nil {
		return false, nil
	}
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		return false, fmt.Errorf("git init: %w (%s)", err, string(out))
	}
	return true, nil
}

func commitVCSBaseline(dir string) error {
	add := exec.Command("git", "-C", dir, "add", "-A")
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w (%s)", err, string(out))
	}
	commit := exec.Command("git", "-C", dir,
		"-c", "user.name=driver-eval",
		"-c", "user.email=driver-eval@example.invalid",
		"commit", "-m", "eval-baseline")
	if out, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w (%s)", err, string(out))
	}
	return nil
}

// setupMutationCheck rejects Setup changes to paths tracked by HEAD and returns
// only newly-created untracked entries for the oracle to conditionally ignore.
func setupMutationCheck(ctx context.Context, dir, before string) (map[string]TreeEntry, error) {
	after, err := vcs.WriteTree(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot after setup: %w", err)
	}
	head, err := gitTreeEntries(ctx, dir, "HEAD")
	if err != nil {
		return nil, err
	}
	beforeEntries, err := gitTreeEntries(ctx, dir, before)
	if err != nil {
		return nil, err
	}
	afterEntries, err := gitTreeEntries(ctx, dir, after)
	if err != nil {
		return nil, err
	}
	for path, entry := range head {
		if afterEntries[path] != entry {
			return nil, fmt.Errorf("setup polluted tracked path %q", path)
		}
	}
	created := make(map[string]TreeEntry)
	for path, entry := range afterEntries {
		if _, tracked := head[path]; !tracked {
			if _, existed := beforeEntries[path]; !existed {
				created[path] = entry
			}
		}
	}
	return created, nil
}

func gitTreeEntries(ctx context.Context, dir, tree string) (map[string]TreeEntry, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "ls-tree", "-rz", tree).Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w", tree, err)
	}
	entries := make(map[string]TreeEntry)
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\t", 2)
		fields := strings.Fields(parts[0])
		if len(parts) != 2 || len(fields) != 3 {
			return nil, fmt.Errorf("invalid git tree entry")
		}
		entries[parts[1]] = TreeEntry{Mode: fields[0], Type: fields[1], Object: fields[2]}
	}
	return entries, nil
}
