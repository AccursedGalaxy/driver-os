package eval

import (
	"context"
	"os"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
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
	Case    string
	Model   string
	RunID   string        // the agent run's stable ID (RunResult.ID) — the P1 spine, so a Trial can be correlated with its persisted transcript.
	Index   int           // 1-based trial number within the cell.
	Outcome agent.Outcome // the agent's self-reported terminal state.
	Answer  string
	Iters   int
	Usage   llm.Usage
	Pass    bool   // the ORACLE's verdict — the actual grade.
	Detail  string // the oracle's evidence.
	Err     string // infra failure (provider/transport), distinct from a non-pass.

	Cost   float64 // USD cost of this trial's Usage at the pinned Pricing (meaningless unless Priced).
	Priced bool    // whether the model had a Pricing entry — false => render "—", not $0.

	// LatencyMs is the agent's wall-clock for this trial (summed model + tool time
	// across steps). Derived from Steps so it survives in report.json after the full
	// trace is dropped — lets the report separate slow runs from token-heavy ones.
	LatencyMs int64

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

// RunTrial executes one trial: materialize a PRISTINE fixture into a fresh temp
// dir (never the live repo, never a reused dir), run the agent against it, then
// grade the resulting on-disk state with the case's oracle. The temp dir is
// removed afterward — the fixture is reproducible, and the verdict is what we
// keep. err-shaped infra failures from the loop are recorded on the Trial (as
// .Err) rather than returned, so one flaky provider call can't abort a whole
// suite sweep; the trial just counts as a non-pass.
func RunTrial(ctx context.Context, c Case, m Model, index int) Trial {
	tr := Trial{Case: c.Name, Model: m.Label, Index: index}

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

	cfg := c.Config // copy the knob template; fill the per-trial fields.
	cfg.Model = m.Provider
	cfg.Sandbox = sb
	cfg.Task = c.Task
	cfg.Root = dir
	cfg.Obs = nil // silent: a trial yields data, not stdout (that's the Observer seam).

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

	// Pick the loop the same way cmd/agent does: native tool-calling when the
	// provider supports it and the case didn't force text, else the text loop.
	run := agent.RunNative
	if c.Protocol == "text" || !m.Provider.Capabilities().Tools {
		run = agent.Run
	}

	res, runErr := run(ctx, cfg)
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
	}
	tr.Cost, tr.Priced = CostOf(m.Label, tr.Usage)
	if runErr != nil {
		tr.Err = runErr.Error()
	}

	// Grade the post-run state regardless of how the loop ended — a provider
	// error or a hit-cap simply leaves the fixture unfinished, which the oracle
	// will fail (correctly, and without counting as a false positive since the
	// Outcome is not Answered).
	g := c.Oracle.Grade(ctx, GradeInput{Root: dir, Sandbox: sb, Result: res})
	tr.Pass = g.Pass
	if tr.Detail == "" {
		tr.Detail = g.Detail
	}
	return tr
}
