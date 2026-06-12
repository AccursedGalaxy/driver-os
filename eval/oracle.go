package eval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Oracle grades a finished run against GROUND TRUTH, independent of what the
// agent claimed. This separation is the harness's whole reason to exist: the
// bake-offs (DOGFOOD R9/R10) showed models report Outcome=Answered while the
// build is red, so the agent's self-report and the truth are different signals.
// An Oracle answers only the second question, the same way a human grader did in
// the dogfood rounds — but reproducibly and at N-run scale.
type Oracle interface {
	Grade(ctx context.Context, in GradeInput) Grade
}

// GradeInput is everything an oracle needs to judge a run. Root is the fixture
// dir AFTER the run — the real on-disk effect to inspect. Sandbox is the same
// fence the agent used, so an oracle runs its checks through the identical
// boundary (and rooted at the same dir). Result carries the agent's typed
// outcome and trace: read it, never trust it as the verdict.
type GradeInput struct {
	Root    string
	Sandbox sandbox.Sandbox
	Result  *agent.RunResult
}

// Grade is an oracle's verdict: did the run actually satisfy the case, and why.
// Detail is the evidence — the failing held-out test, the wrong answer — so a
// failed cell in the report is diagnosable without re-running.
type Grade struct {
	Pass   bool
	Detail string

	// NoAttempt marks a run that left NOTHING gradable — e.g. an unchanged
	// working tree (SWE-bench's empty model patch). Distinct from a wrong
	// attempt: best-of-N selection (SelectBest) demotes these below every trial
	// that did work, because an `answered` over a no-op is a confident no-op.
	NoAttempt bool
}

// defaultOracleTimeout bounds an oracle's verification command when none is set,
// independent of the agent's own RunTimeout — grading is the harness's job, not
// the model's, so it gets its own budget.
const defaultOracleTimeout = 60 * time.Second

// CommandOracle passes iff a shell command exits zero in the post-run fixture.
// It is the domain-specific verifier HP-5 calls for where one exists — "go test
// ./...", a build, a property check — run through the sandbox so it sees exactly
// what the agent left on disk. Label names it in multi-oracle Detail output.
type CommandOracle struct {
	Label   string        // short name for the report Detail (e.g. "suite").
	Cmd     string        // the success command; exit 0 == pass.
	Timeout time.Duration // 0 = defaultOracleTimeout.
}

// Grade runs Cmd and passes only on a zero exit. A command that fails to start
// is a non-pass with the start error as Detail (we could not confirm success).
func (o CommandOracle) Grade(ctx context.Context, in GradeInput) Grade {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultOracleTimeout
	}
	res, err := in.Sandbox.Exec(ctx, sandbox.Command{
		Path:    "sh",
		Args:    []string{"-c", o.Cmd},
		Timeout: timeout,
	})
	if err != nil {
		return Grade{Pass: false, Detail: o.tag(fmt.Sprintf("could not run %q: %v", o.Cmd, err))}
	}
	if res.ExitCode != 0 {
		return Grade{Pass: false, Detail: o.tag(fmt.Sprintf("%q exited %d:\n%s", o.Cmd, res.ExitCode, tail(res)))}
	}
	return Grade{Pass: true, Detail: o.tag(fmt.Sprintf("%q passed", o.Cmd))}
}

func (o CommandOracle) tag(s string) string {
	if o.Label == "" {
		return s
	}
	return o.Label + ": " + s
}

// HeldOut is the suite-green-≠-correct guard (DOGFOOD R9/R10): it Materializes
// extra files the agent NEVER saw — novel/adversarial cases — into the finished
// fixture, then runs Verify over the augmented suite. A run that overfit the
// handed tests (grok's parser that accepted "2+3$") passes the original suite
// but fails here. Drop is additive (it only adds files), so the original suite
// still runs alongside the held-out cases.
type HeldOut struct {
	Label   string        // names this oracle in Detail (e.g. "held-out").
	Drop    Fixture       // the held-out files, written into Root after the run.
	Verify  string        // the command run over the augmented fixture; exit 0 == pass.
	Timeout time.Duration // 0 = defaultOracleTimeout.
}

// Grade drops the held-out files then defers to a CommandOracle on Verify.
func (o HeldOut) Grade(ctx context.Context, in GradeInput) Grade {
	label := o.Label
	if label == "" {
		label = "held-out"
	}
	if err := o.Drop.Materialize(in.Root); err != nil {
		return Grade{Pass: false, Detail: label + ": could not drop held-out files: " + err.Error()}
	}
	return CommandOracle{Label: label, Cmd: o.Verify, Timeout: o.Timeout}.Grade(ctx, in)
}

// AllOf passes iff every oracle passes, evaluated IN ORDER and short-circuiting
// on the first failure (whose Detail it returns). Order matters: a CommandOracle
// on the original suite placed before a HeldOut runs while the held-out files
// are not yet present, so "passed the suite but failed held-out" and "failed the
// suite" stay distinguishable in the report — the overfit signal vs the
// never-solved signal.
func AllOf(oracles ...Oracle) Oracle { return allOf(oracles) }

type allOf []Oracle

func (a allOf) Grade(ctx context.Context, in GradeInput) Grade {
	var details []string
	for _, o := range a {
		g := o.Grade(ctx, in)
		if !g.Pass {
			return Grade{Pass: false, Detail: g.Detail}
		}
		details = append(details, g.Detail)
	}
	return Grade{Pass: true, Detail: strings.Join(details, "; ")}
}

// tail renders the most useful slice of a command's output for a failure Detail:
// stderr if present (where a test/build failure lands), else stdout, clipped so
// a giant log can't bloat the report.
func tail(res *sandbox.Result) string {
	out := string(res.Stderr)
	if strings.TrimSpace(out) == "" {
		out = string(res.Stdout)
	}
	const cap = 1500
	r := []rune(out)
	if len(r) > cap {
		return "…" + string(r[len(r)-cap:])
	}
	return out
}
