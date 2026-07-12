package headless

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/profile"
)

// worktreeValue is the tri-state -worktree flag: 'auto' (the default —
// isolate when the run starts inside a git repo, unless -review needs the
// live tree), or an explicit 'true'/'false'. It implements IsBoolFlag so the
// bare form -worktree still means force-on.
type worktreeValue struct {
	set bool
	val bool
}

func (w *worktreeValue) String() string {
	if !w.set {
		return "auto"
	}
	return strconv.FormatBool(w.val)
}

func (w *worktreeValue) Set(s string) error {
	if s == "auto" {
		w.set = false
		w.val = false
		return nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("want 'auto', 'true', or 'false'")
	}
	w.set, w.val = true, v
	return nil
}

func (w *worktreeValue) IsBoolFlag() bool { return true }

// resolve turns the tri-state into the effective decision for this run.
func (w *worktreeValue) resolve(inRepo, review bool) bool {
	if w.set {
		return w.val
	}
	return inRepo && !review
}

// auto reports whether the value is still the unforced default.
func (w *worktreeValue) auto() bool { return !w.set }

// resolveEffort maps the -effort flag to the wire value: 'default' (or an
// explicitly empty value) means the provider default; anything else must be a
// known tier.
func resolveEffort(s string) (string, error) {
	switch s {
	case "default", "provider", "":
		return "", nil
	case "minimal", "low", "medium", "high", "xhigh":
		return s, nil
	default:
		return "", fmt.Errorf("unknown -effort %q (want minimal|low|medium|high|xhigh, or 'default' for the provider default)", s)
	}
}

// agentFlags holds every cmd/agent flag as one struct so registration lives in
// ONE function (registerFlags) that both runMain and the usage-grouping test
// drive. Field names match the historical local variable names in runMain.
//
// Cross-binary default divergences from cmd/driver are DELIBERATE and
// documented on each field's registration below (the TUI has a human watching;
// the headless binary runs unattended): -max-iters 40 here vs uncapped there,
// -max-tokens 8192 vs 32768, -finish-nudge 0 vs 5.
type agentFlags struct {
	task               *string
	trust              *string
	useMemory          *bool
	maxIters           *int
	maxTokens          *int
	runTimeout         *time.Duration
	verifyTimeout      *time.Duration
	protocol           *string
	verifyCmd          *string
	verifyBaseline     verifyBaselineValue
	verifyLastRun      *bool
	verifyContinue     *bool
	reviewModel        *string
	bestOf             *int
	selectModel        *string
	reviewRounds       *int
	reviewRequired     *bool
	reviewUnverified   *bool
	reviewOptional     *bool
	requireDiff        *bool
	effort             *string
	codeAct            *bool
	reproFirst         *bool
	reproGate          *bool
	batchReads         *bool
	readWindow         *int
	readOutline        *bool
	promptProfile      *string
	bootContext        *bool
	reviewEffort       *string
	planModel          *string
	planEffort         *string
	testFence          *string
	diffScope          *string
	churnNudge         *int
	finishNudge        *int
	diagnoseCmd        *string
	diagnoseAfterEdits *int
	maxWall            *time.Duration
	maxTotalTokens     *int
	budget             *float64
	allowUnpricedSpend *bool
	review             *bool
	worktree           worktreeValue
	keepWorktree       *bool
	sandboxKind        *string
	runtime            *string
	network            *bool
	untrusted          *bool
	session            *bool
	transcriptDir      *string
	format             *string
	trace              *string
	traceFile          *string
	report             *string
	ledger             *bool
	providerFlag       *string
	modelFlag          *string
	ladderFile         *string
	skillsFlag         *string
	approve            *string
	reviewAction       *string
}

// registerFlags defines every cmd/agent flag on fs and returns the bound
// struct. Keep each new flag listed in usageGroups (usage.go) — the
// TestUsageGroupsComplete pin fails otherwise.
func registerFlags(fs *flag.FlagSet) *agentFlags {
	return registerFlagsWithProfile(fs, mustCoding())
}

// registerFlagsWithProfile defines every headless flag using e for the
// execution-profile-covered defaults. CLI values parsed after registration take
// precedence over these defaults.
// profileOverrides includes only flags the user actually supplied. Registration
// defaults are help UX, not overrides.
func profileOverrides(fs *flag.FlagSet, f *agentFlags) profile.Overrides {
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	o := profile.Overrides{}
	if set["max-iters"] {
		o.MaxIters = f.maxIters
	}
	if set["max-tokens"] {
		o.MaxTokens = f.maxTokens
	}
	if set["finish-nudge"] {
		o.FinishNudge = f.finishNudge
	}
	if set["run-timeout"] {
		o.RunTimeout = f.runTimeout
	}
	if set["effort"] {
		o.Effort = f.effort
	}
	if set["verify-continue"] {
		o.VerifyContinue = f.verifyContinue
	}
	if set["memory"] {
		o.Memory = f.useMemory
	}
	if set["boot-context"] {
		o.BootContext = f.bootContext
	}
	if set["batch-reads"] {
		o.BatchReads = f.batchReads
	}
	if set["read-window"] {
		o.ReadWindow = f.readWindow
	}
	if set["read-outline"] {
		o.ReadOutline = f.readOutline
	}
	if set["churn-nudge"] {
		o.ChurnNudgeRuns = f.churnNudge
	}
	if set["require-diff"] {
		o.RequireDiff = f.requireDiff
	}
	if set["worktree"] {
		v := f.worktree.String()
		o.Worktree = &v
	}
	return o
}

func traceProvenance(t profile.Trace) map[string]string {
	out := make(map[string]string, len(t.Fields))
	for key, value := range t.Fields {
		out[key] = string(value.Source)
	}
	return out
}

func registerFlagsWithProfile(fs *flag.FlagSet, e profile.Exec) *agentFlags {
	f := &agentFlags{}
	fs.String("profile", "coding-v2", "execution profile supplying this run's defaults (closed registry: coding-v1, coding-v2, interactive-v1, interactive-v2, observe-v1, eval-swe-v1); explicitly-set flags override the profile — see docs/specs/PROFILES.md")
	f.task = fs.String("task", "What module path and Go version does this project declare? Use the tools to find out, then answer.", "the goal for the agent; '-' reads it from stdin")
	f.trust = fs.String("trust", "", "trust profile for this run (REQUIRED): trusted-local | reviewed-local | container | untrusted — see docs/specs/PROFILES.md")
	f.useMemory = fs.Bool("memory", e.Memory, "recall/store long-term memory across runs (mneme); needs OPENROUTER_API_KEY")
	f.maxIters = fs.Int("max-iters", e.MaxIters, "hard cap on think->act->observe turns (raise for longer tasks; default suits coding tasks)")
	// Defaults to agent.DefaultMaxTokens (8192), so the headless binary tracks the
	// library fallback instead of imposing a lower cap. A too-low cap is dangerous:
	// a 1024 cap strangled a post-feedback reasoning turn (measured 2026-07-02 —
	// task3 hit exactly-1024 completion, all reasoning, empty content → a false
	// "empty final answer").
	f.maxTokens = fs.Int("max-tokens", e.MaxTokens, "per-turn model output cap (raise for larger write/edit blocks)")
	// 60s matches cmd/driver; the old 30s default was the single most overridden
	// footgun in headless recipes (every test suite outlives 30s).
	f.runTimeout = fs.Duration("run-timeout", e.RunTimeout, "wall-clock kill for a single `run` command (e.g. 3m for a slow test suite)")
	f.verifyTimeout = fs.Duration("verify-timeout", 0, "wall-clock bound for closing VerifyCmd executions separately from -run-timeout; 0 = max(-run-timeout, 5m). Raise this when your verify suite (e.g. 'go test ./...') is slower than an interactive command")
	f.protocol = fs.String("protocol", "tools", "model protocol: 'tools' (native function-calling) or 'text' (one-line parsed)")
	f.verifyCmd = fs.String("verify-cmd", "", "success command re-run before accepting a final answer; a non-zero exit marks the run 'unverified' (e.g. 'go test ./...')")
	fs.Var(&f.verifyBaseline, "verify-baseline", "before the first model call, run -verify-cmd once on the untouched workspace: 'on' (default, warn if red), 'off' (skip baseline check), 'abort' (refuse to start if red), 'true'/'false' aliases for on/off; note: use the =value form, e.g. -verify-baseline=abort")
	f.verifyLastRun = fs.Bool("verify-last-run", false, "without -verify-cmd, mark a silent finish 'unverified' if the most recent run command was still failing")
	// Default ON to match cmd/driver: a red verify becoming more work (bounded by
	// -max-iters) beats a dumped unverified answer; -verify-continue=false restores
	// stop-on-first-red.
	f.verifyContinue = fs.Bool("verify-continue", e.VerifyContinue, "with -verify-cmd, feed a failing verification back and keep working (instead of stopping) while iterations remain")
	f.reviewModel = fs.String("review-model", "", "closing REVIEW GATE (docs/specs/REVIEW-GATE.md): an independent reviewer model id (e.g. 'openai/gpt-5.5') run after the fence and -verify-cmd pass; grounded blocking findings are fed back for repair, then mark the run 'unverified'. Empty = gate off")
	f.bestOf = fs.Int("best-of", 1, "Best-of-N solving: run N independent attempts in detached worktrees, then select the best patch with a comparative double-blind selector (1 = off); with -review-model set, attempts run without review and the review gate runs once on the selected winner only")
	f.selectModel = fs.String("select-model", "", "selector model for -best-of comparative double-blind selection; empty reuses -review-model")
	f.reviewRounds = fs.Int("review-rounds", agent.DefaultReviewRounds, "with -review-model, max reviewer rounds before remaining blockers mark the run 'unverified'")
	f.reviewRequired = fs.Bool("review-required", false, "with -review-model, enforce reviewer verdict availability after the gate runs; reviewer unavailable/parse_error/timeout marks the run unverified (distinct from gate viability, which fails setup unless -review-optional is set)")
	f.reviewUnverified = fs.Bool("review-unverified", true, "run a single advisory review round on runs that end unverified with a non-empty diff; findings are recorded in the result but never change the outcome (disable with -review-unverified=false)")
	f.reviewOptional = fs.Bool("review-optional", false, "with -review-model, allow the run to continue if the review gate cannot arm (no git root or baseline capture failure). Default is fail-closed because silent review skips have corrupted measurements")
	f.requireDiff = fs.Bool("require-diff", false, "require a non-empty final git diff versus the run baseline; an empty completion is unverified. Requires a git workspace and is off by default for legitimate no-op tasks")
	// 'low' is the measured default (docs/findings/EFFORT-BELOW-DEFAULT.md: −65–75%
	// reasoning tokens, no resolve regression, suppresses over-scoping). Escalate a
	// hard task by swapping the SOLVER model up, not by cranking effort.
	f.effort = fs.String("effort", e.Effort, "reasoning effort for the solver's model calls (minimal|low|medium|high|xhigh; 'default' = provider default)")
	f.codeAct = fs.Bool("codeact", false, "EXPERIMENTAL: append a code-as-action instruction block to the native system prompt (docs/specs/CODEACT-SCREEN.md)")
	f.reproFirst = fs.Bool("repro-first", false, "EXPERIMENTAL: require a failing reproduction test before the fix — the agent must declare a new test file + command, validated red on the base tree and green on the final tree")
	f.reproGate = fs.Bool("repro-gate", false, "EXPERIMENTAL: append a reproduce-first instruction block to the solver system prompt (trace each relied-on value to its producer and reproduce before answering)")
	f.batchReads = fs.Bool("batch-reads", e.BatchReads, "steer the model to batch independent read-only tool calls into one turn (parallel prefetch); on by default, -batch-reads=false to disable")
	f.readWindow = fs.Int("read-window", 0, "max lines a range-less read_file returns before clipping (0 = default 150)")
	f.readOutline = fs.Bool("read-outline", false, "append a file structure outline (symbols + line numbers) to a clipped read_file so the model can jump to the right range")
	f.promptProfile = fs.String("prompt-profile", "", "base system prompt for the native loop: '' or 'legacy' (minimal historical prompt), 'structured' (working-rules prompt), or 'auto' (route by model family); see docs/specs/PROMPT-SKILLS.md")
	f.bootContext = fs.Bool("boot-context", e.BootContext, "include a Go dependency digest (module + pinned direct-dep versions) in the model's opening context (measured redundant with the model's own go.mod habits, so default off)")
	f.reviewEffort = fs.String("review-effort", "", "reasoning effort for the -review-model reviewer's calls (same tiers; empty = provider default)")
	f.planModel = fs.String("plan-model", "", "opening PLAN STAGE (triad): a planner model id run read-only before the solver's first turn; its plan rides into the task ('provider:model' targets another backend). Empty = stage off")
	f.planEffort = fs.String("plan-effort", "", "reasoning effort for the -plan-model planner's calls (same tiers; empty = provider default)")
	f.testFence = fs.String("test-fence", "", "comma-separated READ-ONLY globs (recommended '*_test.go,testdata/**'): write/edit tools refuse matching paths, and ANY drift at a closing gate (shell redirects included) marks the run 'unverified'. Empty auto-arms the canonical test fence when -verify-cmd is set; -test-fence=off or none opts out; -diff-scope suppresses the auto-fence")
	f.diffScope = fs.String("diff-scope", "", "comma-separated WRITABLE-ONLY globs (the inverse of -test-fence): write/edit tools refuse paths outside this set, and any change outside it at a closing gate (shell redirects included) marks the run 'scope_violation'. Prevents a solver from silently editing code outside the declared task scope, e.g. rewriting production code to fit a guard test. Empty = off")
	f.churnNudge = fs.Int("churn-nudge", 0, "after this many failing `run` results or edits, hint once to rewrite the whole file with write_file (0 = off)")
	f.finishNudge = fs.Int("finish-nudge", e.FinishNudge, "within this many turns of the iteration cap, if the last run was green and files are stable, hint once to wrap up and answer (0 = off)")
	f.diagnoseCmd = fs.String("diagnose-cmd", "", "code-intel slice 1: a compile/type-check command (e.g. 'go build ./...') run when the model is stuck with a broken build; its errors are surfaced as info, not a gate")
	f.diagnoseAfterEdits = fs.Int("diagnose-after-edits", 0, "fire -diagnose-cmd once the agent has edited this many times without a green build (0 = off; -diagnose-cmd also required)")
	f.maxWall = fs.Duration("max-wall", 0, "wall-clock budget for the whole run, checked between turns (e.g. 5m); 0 = off, only the iteration cap bounds it")
	f.maxTotalTokens = fs.Int("max-total-tokens", 0, "cumulative token budget (prompt+completion, summed across turns), checked between turns; 0 = off")
	f.budget = fs.Float64("budget", 0, "cumulative dollar budget (USD) for the run, checked at the turn boundary after a paid generation; 0 = off. Requires a priceable solver model.")
	f.allowUnpricedSpend = fs.Bool("allow-unpriced-spend", false, "allow a configured dollar budget to continue when spend cannot be priced (default fails closed)")
	f.review = fs.Bool("review", false, "work on the current git repo behind a confirm gate (`run` commands need approval) and review the agent's changes as a diff before any commit; requires a clean working tree")
	if e.Worktree == "off" {
		f.worktree.set = true
		f.worktree.val = false
	}
	fs.Var(&f.worktree, "worktree", "run in a throwaway detached git worktree snapshot of the current checkout; any resulting diff is written next to the transcript as <run-id>.patch. 'auto' (default) = on inside a git repo unless -review is set; 'true'/'false' force it")
	f.keepWorktree = fs.Bool("keep-worktree", false, "with -worktree, keep the detached worktree after banking a patch for inspection (default removes it; -review keeps it until review finishes)")
	f.sandboxKind = fs.String("sandbox", "local", "isolation backend: 'local' (host subprocess, TRUSTED code only) or 'docker' (container: network off, resource-capped, root fs read-only)")
	f.runtime = fs.String("runtime", "runc", "container runtime for -sandbox=docker: 'runc' (shared host kernel = process isolation) or 'runsc' (gVisor userspace kernel = kernel isolation)")
	f.network = fs.Bool("network", false, "allow network access inside the sandbox (-sandbox=docker only); off by default so untrusted code can't exfiltrate")
	f.untrusted = fs.Bool("untrusted", false, "treat the task's code as HOSTILE: require kernel-level isolation (gVisor) and refuse to start on anything weaker (so `run` never executes untrusted code on the host or a plain container)")
	f.session = fs.Bool("session", false, "give the model's `run` tool a STATEFUL shell: `cd`/`export` persist across commands. Verify/diagnose still run clean. Opt-in; see docs/specs/SESSION.md")
	f.transcriptDir = fs.String("transcript-dir", agent.TranscriptDir(), "directory to persist the run transcript (<id>.json + runs.jsonl index); empty disables it")
	f.format = fs.String("format", "text", "output format: 'text' (human SUMMARY + answer on stdout), 'json' (one result object on stdout), or 'ndjson' (one event per turn + a terminal result event); the live trace always goes to stderr")
	f.trace = fs.String("trace", "full", "stderr trace verbosity: 'full' (every model/observation line) or 'compact' (one line per iteration + gate milestones — the orchestrator-friendly heartbeat)")
	f.traceFile = fs.String("trace-file", "", "also write the FULL live trace to this file, regardless of -trace (debug a compact run from the file, not the terminal)")
	f.report = fs.String("report", "", "write a one-read markdown orchestrator report (result, answer, diff, next steps) to this file at the end of the run")
	f.ledger = fs.Bool("ledger", true, "append one JSONL record per run to the delegation ledger ($DRIVER_OS_LEDGER_DIR or ~/.local/state/driver-os/ledger.jsonl); -ledger=false disables")
	f.providerFlag = fs.String("provider", "", "LLM provider: '' (auto: infer from whichever key is set), 'openrouter', 'xai', 'openai', or 'ollama'")
	f.modelFlag = fs.String("model", "", "model id, overriding the provider's *_MODEL env default (e.g. 'openai/gpt-4o' on openrouter)")
	f.ladderFile = fs.String("ladder", "", "JSON ladder config; when set, -model is ignored and rung models are used")
	f.skillsFlag = fs.String("skills", "", "extra skill dirs to load, comma-separated (each a skill folder or a parent of skill folders); 'none' disables skills entirely. User (~/.config/driver-os/skills) and project (.agents/skills) skills load automatically; see docs/specs/SKILLS.md")
	f.approve = fs.String("approve", "interactive", "with -review, how to decide a gated `run` the policy doesn't auto-allow: 'interactive' (ask on the terminal), 'policy' (auto-allow the safe-prefix allowlist, block the rest), or 'never' (block every `run`)")
	f.reviewAction = fs.String("review-action", "prompt", "with -review, what to do with the agent's diff at the end: 'prompt' (ask on the terminal), 'commit', 'discard', or 'keep' (leave unstaged)")
	return f
}
