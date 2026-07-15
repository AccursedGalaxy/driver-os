// Command agent is the CLI front-end for the package agent loop. The loop itself
// — think -> act -> observe, the tools, the termination conditions — lives in
// ../../agent so it can be both driven here and MEASURED by the eval harness
// (HP-11). This file is only the wiring: pick a provider, open the sandbox and
// memory, run, and render the result to the terminal.
package headless

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/AccursedGalaxy/driver-os/memory"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/agent/skill"
	"github.com/AccursedGalaxy/driver-os/cli"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/gated"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

// setupLedger holds the parsed CLI context needed when a failure occurs before
// a RunResult exists. It is configured by runMain after flag parsing; appending
// is deliberately best-effort so setup-error output remains byte-for-byte the
// normal CLI error contract.
var setupLedger struct {
	enabled bool
	model   string
	task    string
}

func configureSetupLedger(enabled bool, model, task string) {
	setupLedger.enabled = enabled
	setupLedger.model = model
	setupLedger.task = task
}

func setSetupLedgerTask(task string) { setupLedger.task = task }

// loadDotenv is replaceable in tests so trust refusals can prove they never load
// repository-local .env content.
var loadDotenv = func() { _ = godotenv.Load() }

// taskStdin keeps stdin consumption behind the trust-consent boundary and makes
// that boundary observable without changing normal CLI behavior.
var taskStdin io.Reader = os.Stdin

func appendSetupErrorLedger() {
	if !setupLedger.enabled {
		return
	}
	repo, _ := os.Getwd()
	_ = agent.AppendLedger(agent.RunRecord{
		SchemaVersion: agent.TranscriptSchemaVersion,
		Model:         setupLedger.model,
		Task:          setupLedger.task,
		Outcome:       agent.Outcome("setup_error"),
	}, agent.LedgerMeta{Repo: repo, ExitCode: 1})
}

var canonicalTestFence = []string{"*_test.go", "testdata/**", "test_*.py", "*_test.py", "tests/**", "conftest.py"}

func resolveTestFence(flagValue, verifyCmd string, diffScope []string) []string {
	trimmed := strings.TrimSpace(flagValue)
	if strings.EqualFold(trimmed, "off") || strings.EqualFold(trimmed, "none") {
		return nil
	}
	if trimmed != "" {
		return cli.SplitList(flagValue)
	}
	if strings.TrimSpace(verifyCmd) == "" || len(diffScope) > 0 {
		return nil
	}
	return append([]string(nil), canonicalTestFence...)
}

func Main(argv []string) int {
	return MainWithExtras(argv, agent.InvocationSurfaceDriverRun, Extras{})
}

// MainWithInvocation runs headless mode with the dispatcher-selected invocation
// surface. It receives this as data rather than inspecting process arguments.
func MainWithInvocation(argv []string, invocationSurface string) int {
	return MainWithExtras(argv, invocationSurface, Extras{})
}

// MainWithExtras runs headless mode with optional binary-supplied capabilities.
func MainWithExtras(argv []string, invocationSurface string, extras Extras) int {
	fs := flag.NewFlagSet("driver-agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return runMainWithInvocationExtras(fs, argv, invocationSurface, extras)
}

func effectiveReviewRequired(explicitlySet bool, flagVal bool, reviewModel string) (reviewRequired bool, reviewFailOpen bool, err error) {
	if explicitlySet && flagVal && reviewModel == "" {
		return false, false, errors.New("-review-required requires -review-model; a required review with no reviewer configured is a misconfiguration")
	}
	if explicitlySet && !flagVal && reviewModel != "" {
		return false, true, nil
	}
	if explicitlySet {
		return flagVal, false, nil
	}
	return false, false, nil
}

func budgetCostFn(modelLabel string, prices ...PriceLookup) func(llm.Usage) (float64, bool) {
	return func(u llm.Usage) (float64, bool) {
		if len(prices) == 0 || prices[0] == nil {
			return 0, false
		}
		return prices[0](modelLabel, u)
	}
}

func wireBudgetControls(cfg *agent.Config, maxTotalCostUSD float64, modelLabel string, prices ...PriceLookup) {
	cfg.MaxTotalCostUSD = maxTotalCostUSD
	cfg.CostFn = budgetCostFn(modelLabel, prices...)
	cfg.SolverModel = modelLabel
	cfg.Spend = agent.NewSpend(func(model string, u llm.Usage) (float64, bool) {
		if u.Cost > 0 {
			return u.Cost, true
		}
		if len(prices) == 0 || prices[0] == nil {
			return 0, false
		}
		return prices[0](model, u)
	})
}

// runMain is the real entry point. It returns an exit code instead of calling
// os.Exit directly so that DEFERS run on every path — critically sb.Close(),
// which reaps the docker backend's container. os.Exit skips defers, so a refused
// or failed run under -sandbox=docker would otherwise LEAK a container (the
// container is started in docker.New, before the isolation gate can refuse).
func runMain(fs *flag.FlagSet, argv []string) int {
	return runMainWithInvocationExtras(fs, argv, agent.InvocationSurfaceDriverRun, Extras{})
}

func runMainWithInvocation(fs *flag.FlagSet, argv []string, invocationSurface string) int {
	return runMainWithInvocationExtras(fs, argv, invocationSurface, Extras{})
}

func runMainWithInvocationExtras(fs *flag.FlagSet, argv []string, invocationSurface string, extras Extras) int {
	execProfile, profileErr := profileFromArgs(argv)
	if profileErr != nil {
		execProfile = mustCoding()
	}
	f := registerFlagsWithProfile(fs, execProfile)
	fs.Usage = cli.GroupedUsage(fs, os.Stderr, usageHeader, usageGroups)
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	configureSetupLedger(*f.ledger, *f.modelFlag, *f.task)

	// Resolve the output format up front: every later setup error needs it to
	// decide between a stderr line (text) and a cli_error object on stdout (json).
	// A flag-parse failure BEFORE this point is handled by the flag package (exit 2,
	// stderr) — acceptable, since there is no resolved format to honor (D3).
	outFmt, ferr := validateFormat(*f.format)
	if ferr != nil {
		fmt.Fprintln(errW, ferr)
		return 1
	}
	if profileErr != nil {
		return failSetup(os.Stdout, errW, outFmt, "profile", profileErr.Error())
	}
	if perr := validateProtocol(*f.protocol); perr != nil {
		return failSetup(os.Stdout, errW, outFmt, "bad_protocol", perr.Error())
	}

	// Consent boundary (docs/specs/PROFILES.md §1.1): apart from in-memory
	// setup-ledger configuration, above this point only flag parsing and
	// output-format, protocol, and execution-profile validation may run.
	// A missing or invalid trust profile must be refused before ambient
	// environment or repository content can influence this process.
	explicit := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })
	tr, err := resolveTrust(*f.trust)
	if err != nil {
		return failSetup(os.Stdout, errW, outFmt, "trust", err.Error())
	}
	if err := profile.RequireTrust(execProfile, tr); err != nil {
		return failSetup(os.Stdout, errW, outFmt, "trust", err.Error())
	}
	plan, err := planTrust(tr, explicit, f)
	if err != nil {
		return failSetup(os.Stdout, errW, outFmt, "trust", err.Error())
	}

	// Trust consent succeeded; environment, repository, and runtime setup may
	ov := profileOverrides(fs, f)
	resolved, resolutionTrace, err := profile.Resolve(profile.TrustPosture{Selected: plan.Trust, Posture: profile.FloorFor(plan.Trust), Surface: invocationSurface}, execProfile, ov)
	if err != nil {
		return failSetup(os.Stdout, errW, outFmt, "profile", err.Error())
	}
	// now begin.
	loadDotenv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Route the stderr trace (D1 diagnostics channel) through the -trace mode:
	// 'compact' turns the full model/observation stream into a one-line-per-
	// iteration heartbeat, and -trace-file banks the unfiltered stream either
	// way. Every later stderr write in this package goes through errW.
	tw, terr := setupTrace(*f.trace, *f.traceFile)
	if terr != nil {
		return failSetup(os.Stdout, errW, outFmt, "invalid_flags", terr.Error())
	}
	if tw != nil {
		defer tw.Close()
	}

	// The solver's reasoning effort defaults to 'low' (measured: −65–75%
	// reasoning tokens, no resolve regression — docs/findings/
	// EFFORT-BELOW-DEFAULT.md); 'default' (or an explicitly empty value)
	// restores the provider default.
	effortVal, eerr := resolveEffort(*f.effort)
	if eerr != nil {
		return failSetup(os.Stdout, errW, outFmt, "invalid_flags", eerr.Error())
	}
	_ = effortVal // validation is retained; Resolve supplies the effective value.

	// Resolve the task (D6). `-task -` reads it from stdin (so a heredoc or file can
	// drive the agent). Under a machine format an OMITTED -task is an error rather
	// than the canned demo default — a script must not silently run the demo; text
	// mode keeps the default. An empty task is always an error.
	taskSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "task" {
			taskSet = true
		}
	})
	taskVal := *f.task
	switch {
	case taskVal == "-":
		data, rerr := io.ReadAll(taskStdin)
		if rerr != nil {
			return failSetup(os.Stdout, errW, outFmt, "internal", "reading task from stdin: "+rerr.Error())
		}
		taskVal = strings.TrimSpace(string(data))
	case !taskSet && outFmt != formatText:
		return failSetup(os.Stdout, errW, outFmt, "task_required",
			"-task is required under -format="+string(outFmt)+" (no canned default in a machine format); pass -task <goal> or pipe one with -task -")
	}
	setSetupLedgerTask(taskVal)
	if strings.TrimSpace(taskVal) == "" {
		return failSetup(os.Stdout, errW, outFmt, "empty_task",
			"the task is empty — pass -task <goal>, or pipe one with -task -")
	}

	if err := validateReviewFlags(*f.approve, *f.reviewAction); err != nil {
		return failSetup(os.Stdout, errW, outFmt, "invalid_flags", err.Error())
	}

	reviewRequiredExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "review-required" {
			reviewRequiredExplicit = true
		}
	})
	effectiveReviewRequired, reviewFailOpen, err := effectiveReviewRequired(reviewRequiredExplicit, *f.reviewRequired, *f.reviewModel)
	if err != nil {
		return failSetup(os.Stdout, errW, outFmt, "invalid_flags", err.Error())
	}

	ladderMode := strings.TrimSpace(*f.ladderFile) != ""

	var ladderCfg LadderConfig
	var rerr error
	if ladderMode {
		if extras.LoadLadder == nil || extras.RunLadder == nil {
			return failSetup(os.Stdout, errW, outFmt, "ladder", "-ladder: not available in this build")
		}
		ladderCfg, rerr = extras.LoadLadder(*f.ladderFile)
		if rerr != nil {
			return failSetup(os.Stdout, errW, outFmt, "ladder", rerr.Error())
		}
		if *f.bestOf >= 2 {
			return failSetup(os.Stdout, errW, outFmt, "invalid_flags", "-ladder and -best-of are incompatible multi-attempt orchestrators")
		}
		if *f.modelFlag != "" {
			fmt.Fprintln(errW, "ladder: -model ignored; rung models are used")
		}
	}

	// roleProvider resolves a role's model ref ("provider:model" or bare id;
	// bare falls back to -provider) into its OWN provider handle, so a role
	// can sit on a different backend than the solver.
	roleProvider := func(ref string) (llm.Provider, error) {
		kind, id := cli.SplitModelRef(ref)
		if kind == "" {
			kind = *f.providerFlag
		}
		return cli.PickProvider(kind, id)
	}

	var model llm.Provider
	if !ladderMode {
		var perr error
		model, perr = cli.PickProvider(*f.providerFlag, *f.modelFlag)
		if perr != nil {
			kind := "provider"
			if errors.Is(perr, cli.ErrNoProviderKey) {
				kind = "no_provider_key"
			}
			return failSetup(os.Stdout, errW, outFmt, kind, perr.Error())
		}
	}

	// The review gate's independent reviewer (docs/specs/REVIEW-GATE.md): a SECOND provider
	// handle pinned to -review-model, injected as agent.Config.Reviewer. Same-
	// model review is the one arrangement measured useless-to-harmful, so warn —
	// but don't refuse; the eval protocol deliberately measures it.
	var reviewer agent.Reviewer
	if *f.reviewModel != "" && !ladderMode {
		if extras.NewReviewer == nil {
			return failSetup(os.Stdout, errW, outFmt, "review_model", "-review-model: not available in this build")
		}
		rp, rerr := roleProvider(*f.reviewModel)
		if rerr != nil {
			return failSetup(os.Stdout, errW, outFmt, "review_model", "review-model: "+rerr.Error())
		}
		if mp, ok := model.(interface{ Model() string }); ok && mp.Model() == *f.reviewModel {
			fmt.Fprintln(errW, "review gate: WARNING — the review model equals the solver model; same-model self-review is measured useless-to-harmful (docs/specs/REVIEW-GATE.md finding 2). Pick a different -review-model.")
		}
		reviewer = extras.NewReviewer(rp, *f.reviewModel, *f.reviewEffort)
		fmt.Fprintf(errW, "review gate: ON — model=%s rounds=%d\n", *f.reviewModel, *f.reviewRounds)
	}
	// The plan stage's planner (triad): a THIRD provider handle pinned to
	// -plan-model, injected as agent.Config.Planner. Fails open in the loop —
	// a planner fault never blocks the run.
	var planner agent.Planner
	if *f.planModel != "" {
		if extras.NewPlanner == nil {
			return failSetup(os.Stdout, errW, outFmt, "plan_model", "-plan-model: not available in this build")
		}
		pp, perr := roleProvider(*f.planModel)
		if perr != nil {
			return failSetup(os.Stdout, errW, outFmt, "plan_model", "plan-model: "+perr.Error())
		}
		planner = extras.NewPlanner(pp, *f.planModel, *f.planEffort)
		eff := *f.planEffort
		if eff == "" {
			eff = "(default)"
		}
		fmt.Fprintf(errW, "plan stage: ON — model=%s effort=%s\n", *f.planModel, eff)
	}
	scope := cli.SplitList(*f.diffScope)
	fence := resolveTestFence(*f.testFence, *f.verifyCmd, scope)
	if len(fence) > 0 {
		if strings.TrimSpace(*f.testFence) == "" && strings.TrimSpace(*f.verifyCmd) != "" && len(scope) == 0 {
			fmt.Fprintf(errW, "test fence: AUTO-ARMED — %s (read-only; opt out with -test-fence=off)\n", strings.Join(fence, ","))
		} else {
			fmt.Fprintf(errW, "test fence: ON — %s (read-only; drift marks the run unverified)\n", strings.Join(fence, ","))
		}
	}
	if len(scope) > 0 {
		fmt.Fprintf(errW, "diff scope: ON — %s (writable-only; changes outside this set are a scope_violation)\n", strings.Join(scope, ","))
	}

	// Validate the normalized sandbox posture before doing any work. Legacy
	// safety flags have already been composed into planTrust; they are not
	// independent runtime policy switches.
	if err := cli.ValidateSandboxFlags(plan.SandboxKind, plan.Runtime, plan.Network, false); err != nil {
		return failSetup(os.Stdout, errW, outFmt, "invalid_flags", err.Error())
	}

	// The sandbox is the isolation boundary every effect flows through (see
	// ../../sandbox + ../../docs/specs/SANDBOX.md). -sandbox picks the backend: `local`
	// (IsolationNone — host subprocess, TRUSTED code only) or `docker` (a
	// resource-capped container, network off, gVisor via -runtime=runsc). We log
	// the real isolation level so the trust boundary is never invisible.
	cwd, err := os.Getwd()
	if err != nil {
		return failSetup(os.Stdout, errW, outFmt, "internal", "getwd: "+err.Error())
	}
	origCwd := cwd
	bestOfSelectModel := strings.TrimSpace(*f.selectModel)
	if bestOfSelectModel == "" {
		bestOfSelectModel = strings.TrimSpace(*f.reviewModel)
	}

	// -worktree resolves its 'auto' default here: isolate in a git repo unless
	// -review needs the live tree (or the operator forced a value). The ladder
	// path returns before the patch-collection block, so a worktree'd ladder
	// run would silently discard its edits (review 2026-07-07): auto stays OFF
	// under -ladder, and an explicitly forced combination is refused loudly.
	inRepo := vcs.IsRepo(ctx, origCwd)
	worktreeOn := f.worktree.resolve(inRepo, *f.review)
	if plan.ForceWorktree {
		if !inRepo {
			return failSetup(os.Stdout, errW, outFmt, "worktree", "trust profile requires a git worktree, but the current directory is not a git repository")
		}
		worktreeOn = true
	}
	if ladderMode && worktreeOn {
		if !f.worktree.auto() {
			return failSetup(os.Stdout, errW, outFmt, "invalid_flags",
				"-ladder and -worktree are incompatible: the ladder path does not bank a worktree patch, so the run's edits would be discarded; drop -worktree (ladder runs edit the working tree)")
		}
		worktreeOn = false
	}
	if f.worktree.auto() && worktreeOn {
		fmt.Fprintln(errW, "worktree: auto — git repo detected; running isolated (-worktree=false to edit the working tree directly)")
	}

	// finalize is the single sink every finished run flows through — the main
	// path, the ladder path, and best-of all emit here, so the role-model
	// stamp, the delegation ledger, and the -report file cover all of them.
	stampSelectModel := ""
	if *f.bestOf >= 2 {
		stampSelectModel = bestOfSelectModel
	}
	finalize := func(r resultObject, delta *reportDelta) int {
		stampRoleModels(&r, *f.reviewModel, *f.planModel, stampSelectModel)
		attachProofBundle(&r)
		exit := emitResult(os.Stdout, outFmt, r)
		if *f.ledger {
			rec := ledgerRecordFromResult(r)
			reviewModel := ""
			if r.ReviewModel != nil {
				reviewModel = *r.ReviewModel
			}
			hasPatch := r.PatchPath != nil && *r.PatchPath != ""
			if lerr := agent.AppendLedger(rec, agent.LedgerMeta{Repo: origCwd, ExitCode: r.ExitCode, ReviewModel: reviewModel, HasPatch: hasPatch}); lerr != nil {
				fmt.Fprintln(errW, "ledger:", lerr)
			}
		}
		if *f.report != "" {
			if rerr := writeReport(*f.report, r, reportOpts{TraceFile: *f.traceFile, Repo: origCwd, Delta: delta}); rerr != nil {
				fmt.Fprintln(errW, "report:", rerr)
			} else {
				fmt.Fprintln(errW, "report: "+*f.report)
			}
		}
		return exit
	}
	if err := validateBestOfSetup(*f.bestOf, bestOfSelectModel, *f.reviewModel, vcs.IsRepo(ctx, origCwd)); err != nil {
		return failSetup(os.Stdout, errW, outFmt, "best_of", err.Error())
	}
	if *f.bestOf >= 2 {
		sp, serr := roleProvider(bestOfSelectModel)
		if serr != nil {
			return failSetup(os.Stdout, errW, outFmt, "select_model", "select-model: "+serr.Error())
		}
		selector := llmBestOfSelector{Provider: sp, Model: bestOfSelectModel, Effort: *f.reviewEffort}
		return runBestOfCLI(ctx, bestOfCLIOptions{
			N: *f.bestOf, OrigCwd: origCwd, Task: taskVal, Model: model, ModelLabel: modelLabel(model), Selector: selector, SelectorModel: bestOfSelectModel,
			InvocationSurface: invocationSurface, Overrides: ov,
			TrustPlan: plan, Session: *f.session,
			Review: *f.review, KeepWorktree: *f.keepWorktree, Approve: *f.approve, ReviewAction: *f.reviewAction, ReviewRounds: *f.reviewRounds, ReviewRequired: effectiveReviewRequired, ReviewFailOpen: reviewFailOpen, ReviewOptional: *f.reviewOptional, ReviewUnverified: *f.reviewUnverified, RequireDiff: resolved.RequireDiff, Reviewer: reviewer,
			Planner: planner, Memory: *f.useMemory, OpenMemory: extras.OpenMemory, MemoryDBName: extras.MemoryDBName, SkillsFlag: *f.skillsFlag, Protocol: *f.protocol, OutFmt: outFmt, Finalize: finalize, TranscriptDir: *f.transcriptDir,
			MaxIters: resolved.MaxIters, MaxTokens: resolved.MaxTokens, RunTimeout: resolved.RunTimeout, VerifyTimeout: *f.verifyTimeout, VerifyCmd: *f.verifyCmd, SkipVerifyBaseline: f.verifyBaseline.skip, AbortOnRedBaseline: f.verifyBaseline.abort, VerifyLastRun: *f.verifyLastRun, VerifyContinue: resolved.VerifyContinue,
			TestFence: fence, DiffScope: scope, ReasoningEffort: resolved.Effort, PromptProfile: *f.promptProfile, CodeAct: *f.codeAct, ReproFirst: *f.reproFirst, ReproGate: *f.reproGate, BatchReads: resolved.BatchReads, BootContext: resolved.BootContext, ChurnNudge: resolved.ChurnNudgeRuns, FinishNudge: resolved.FinishNudge,
			AutoVerify: resolved.AutoVerify, AutoVerifySoft: resolved.AutoVerifySoft, StandingContext: resolved.StandingContext, NavSpiralWindow: resolved.NavSpiralWindow, AnswerNudgeWindow: resolved.AnswerNudgeWindow,
			DiagnoseCmd: *f.diagnoseCmd, DiagnoseAfterEdits: *f.diagnoseAfterEdits, MaxWall: *f.maxWall, MaxTotalTokens: *f.maxTotalTokens, MaxTotalCostUSD: *f.budget, AllowUnpricedSpend: *f.allowUnpricedSpend, Price: extras.Price,
			ReadWindow: resolved.ReadWindow, ReadOutline: resolved.ReadOutline,
			ExecProfileName: execProfile.Name, ExecProfileHash: execProfile.Hash(), CLIOverrides: cliOverrides(fs), RequiredTrust: string(resolutionTrace.ProfileRequiredTrust), Canonical: resolutionTrace.Canonical, FieldProvenance: traceProvenance(resolutionTrace),
		})
	}
	worktreeDir := ""
	worktreeHandled := false
	var baselineCommit string
	var baselineDirtyFiles []string
	if worktreeOn {
		wi, werr := worktreeAdd(origCwd)
		if werr != nil {
			return failSetup(os.Stdout, errW, outFmt, "worktree", "worktree: "+werr.Error())
		}
		worktreeDir = wi.Dir
		baselineCommit = wi.BaseCommit
		baselineDirtyFiles = wi.DirtyFiles
		defer func() {
			if !worktreeHandled {
				if err := worktreeRemove(worktreeDir); err != nil {
					fmt.Fprintln(errW, "worktree: cleanup after setup failure failed:", err)
				}
			}
		}()
		cwd = wi.Dir
		if len(baselineDirtyFiles) > 0 {
			show := baselineDirtyFiles
			truncated := ""
			if len(show) > 20 {
				show = show[:20]
				truncated = "…"
			}
			fmt.Fprintf(errW, "worktree: %s (baseline %s = dirty-tree snapshot of the main checkout; %d files differ from HEAD: %s%s)\n",
				wi.Dir, baselineCommit, len(baselineDirtyFiles), strings.Join(show, ", "), truncated)
		} else {
			fmt.Fprintf(errW, "worktree: %s (baseline %s = HEAD; main checkout was clean)\n", wi.Dir, baselineCommit)
		}
	}
	sb, err := cli.BuildSandboxWithEnvAllowlist(ctx, plan.SandboxKind, cwd, plan.Runtime, plan.Network, plan.EnvAllowlist)
	if err != nil {
		return failSetup(os.Stdout, errW, outFmt, "sandbox", "sandbox: "+err.Error())
	}
	defer sb.Close()
	caps := sb.Capabilities()
	fmt.Fprintf(errW, "sandbox: %s (isolation=%s, network=%t) — %s\n", plan.SandboxKind, caps.Isolation, caps.Network, cli.TrustLabel(caps.Isolation))

	// The normalized trust plan carries the authoritative isolation floor.
	minIsolation := plan.MinIsolation
	if plan.Trust == profile.Untrusted {
		fmt.Fprintf(errW, "untrusted: ON — requiring isolation >= %s before running any code\n", minIsolation)
	}

	// -session gives the model's `run` tool a stateful shell (cwd/env persist across
	// commands). The session sits BELOW the review gate (so the gate inspects the
	// real command, not the shell wrapper) and the verification gates run on the BASE
	// sandbox in a clean context — see docs/specs/SESSION.md. `toolBase` is what the model's
	// tools act through; `sb` (base) stays the source for verify/diagnose.
	toolBase := sb
	if *f.session {
		sr, ok := sb.(sandbox.Sessioner)
		if !ok {
			return failSetup(os.Stdout, errW, outFmt, "session_unsupported",
				fmt.Sprintf("session: normalized sandbox %s does not support sessions", plan.SandboxKind))
		}
		sess, err := sr.NewSession(ctx)
		if err != nil {
			return failSetup(os.Stdout, errW, outFmt, "session", "session: "+err.Error())
		}
		defer sess.Close() // LIFO: runs before `defer sb.Close()`, while the container is still up.
		toolBase = sessionSandbox{Sandbox: sb, sess: sess}
		fmt.Fprintln(errW, "session: ON — `cd`/`export` persist across `run` commands (verify/diagnose run clean)")
	}

	// -review is the trust layer for working on a REAL repo: require a clean tree
	// (so the post-run diff is unambiguously the agent's), gate `run` commands
	// behind a confirmation prompt, and review the changes before any commit. Off
	// by default — the bare local sandbox edits the working tree directly, as
	// before. With -worktree, cwd is already a fresh detached checkout at HEAD, so
	// this cleanliness requirement composes without special handling. The agent
	// acts through `effects` (the tool chain); verify/diagnose use `verifyEffects`
	// (the base chain, no session).
	gate := func(inner sandbox.Sandbox) sandbox.Sandbox {
		if plan.GatePolicy != nil {
			return gated.New(inner, nil, approvalPolicyAdapter(*plan.GatePolicy))
		}
		return inner
	}
	var tty *ttyIO // opened only when an interactive prompt is actually needed.
	if *f.review {
		clean, err := vcs.IsClean(ctx, cwd)
		if err != nil {
			return failSetup(os.Stdout, errW, outFmt, "review", "review: "+err.Error())
		}
		if !clean {
			return failSetup(os.Stdout, errW, outFmt, "dirty_tree",
				"review: the working tree has uncommitted changes — commit or stash them first so the diff is purely the agent's work")
		}
		// Interactive prompts (the per-run approver and the final action) read from
		// /dev/tty, not stdin — so a task piped on stdin (`-task -`) and an approval
		// prompt never fight over the same stream (D5, closes O3), the way git/ssh do.
		// If an interactive policy is selected with no controlling terminal (CI, cron,
		// a bare pipe), fail fast rather than deadlock on an absent TTY (closes O4).
		if *f.approve == "interactive" || *f.reviewAction == "prompt" {
			t, terr := openTTY()
			if terr != nil {
				return failSetup(os.Stdout, errW, outFmt, "no_tty",
					"review needs an interactive terminal for -approve=interactive / -review-action=prompt, but /dev/tty could not be opened ("+terr.Error()+") — run non-interactively with -approve=policy (or =never) and -review-action=commit|discard|keep")
			}
			tty = t
			defer tty.Close()
		}
		approver, policy := reviewGate(*f.approve, tty)
		trustGate := gate
		gate = func(inner sandbox.Sandbox) sandbox.Sandbox {
			return gated.New(trustGate(inner), approver, policy)
		}
		fmt.Fprintf(errW, "review: ON — approve=%s, on-finish=%s\n", *f.approve, *f.reviewAction)
	}
	effects := gate(toolBase)
	// verifyEffects is the clean (session-free) chain for the verification gates;
	// only meaningfully different when a session is on. nil-equivalent otherwise, so
	// we leave Config.VerifySandbox unset unless -session is in play.
	var verifyEffects sandbox.Sandbox
	if *f.session {
		verifyEffects = gate(sb)
	}

	// Skills (docs/specs/SKILLS.md): discover SKILL.md folders across the user, project, and
	// explicit layers, and — only when any exist — swap in a toolset extended
	// with the single `skill` meta-tool. An empty set leaves Config.Tools nil,
	// so a skill-free run's system prompt stays byte-identical to before.
	// The model's tools act through `effects` (gate/session), but staging copies
	// flow through the BASE sandbox: it is harness-authored content, and routing
	// its mkdir through the review gate would prompt for our own plumbing.
	var tools map[string]agent.Tool
	if *f.skillsFlag != "none" {
		// Project skills are dropped by the authoritative untrusted profile: a
		// hostile checkout must not inject standing instructions.
		projectDir := skill.ProjectDir(cwd)
		if !plan.AllowProjectSkills {
			projectDir = ""
		}
		skills, warns := skill.Discover(skill.Sources{
			UserDir:    skill.UserDir(),
			ProjectDir: projectDir,
			Explicit:   cli.SplitList(*f.skillsFlag),
		})
		for _, w := range warns {
			fmt.Fprintln(errW, "skills:", w)
		}
		if len(skills) > 0 {
			tools = agent.DefaultTools(effects, *f.runTimeout, agent.ReadOptions{Window: *f.readWindow, Outline: *f.readOutline})
			tools["skill"] = skill.Tool(skills, sb, 0)
			names := make([]string, len(skills))
			for i, s := range skills {
				names[i] = s.Name
			}
			fmt.Fprintf(errW, "skills: %d available (%s)\n", len(skills), strings.Join(names, ", "))
		}
	}

	// Long-term memory across runs. Best-effort: nil when no key or no backend,
	// and the agent runs fine without it (Principle 6 — a missing dependency is
	// a degraded mode, not a crash). The backend itself is an injected Extras
	// capability; the recovery guidance for backend-specific failures (e.g. a
	// changed embedder) travels in the returned error's message.
	var mem memory.Store
	if *f.useMemory {
		if extras.OpenMemory == nil {
			fmt.Fprintln(errW, "memory: not available in this build; running stateless")
		} else {
			memoryDBPath := extras.MemoryDBName
			if worktreeOn {
				memoryDBPath = filepath.Join(origCwd, extras.MemoryDBName)
			}
			m, err := extras.OpenMemory(memoryDBPath)
			if err != nil {
				fmt.Fprintln(errW, "memory: setup failed (continuing without it):", err)
			}
			mem = m
			if mem != nil {
				defer mem.Close()
				fmt.Fprintln(errW, "memory: enabled ("+memoryDBPath+")")
			}
		}
	}

	// Default to native tool-calling when the provider supports it (the production
	// path); fall back to the transparent text loop otherwise or on -protocol=text.
	loopFn := agent.Run
	loop := "text"
	fallbackReason := ""
	if !ladderMode {
		if *f.protocol == "tools" && model.Capabilities().Tools {
			loopFn, loop = agent.RunNative, "tools"
		} else if *f.protocol == "tools" {
			fallbackReason = "provider lacks tool capability"
			fmt.Fprintln(errW, "protocol: provider lacks tool support — falling back to text loop")
		}
		fmt.Fprintln(errW, "protocol: "+loop)
	}

	// Observer routing follows D1's stream split. In text/json the live trace is
	// diagnostics → stderr. In ndjson the trace IS the data channel: structured
	// events go to stdout, where the terminal `result` event closes the stream.
	obs := agent.NewWriterObserver(errW)
	if outFmt == formatNDJSON {
		obs = ndjsonObserver{os.Stdout}
	}

	if tools == nil {
		tools = agent.DefaultTools(effects, *f.runTimeout, agent.ReadOptions{Window: *f.readWindow, Outline: *f.readOutline})
	}

	reviewPolicy, _ := agent.ReviewPolicyFrom(effectiveReviewRequired, reviewFailOpen, *f.reviewOptional)
	baseCfg := buildBaseConfig(baseConfigInput{
		InvocationSurface: invocationSurface, RequestedProtocol: *f.protocol, FallbackReason: fallbackReason,
		ExecProfile: execProfile, CLIOverrides: cliOverrides(fs), Model: model, ModelInfo: llm.Lookup(modelLabel(model)),
		Sandbox: effects, VerifySandbox: verifyEffects, Memory: mem, Tools: tools, Task: taskVal, Root: cwd,
		MinIsolation: minIsolation, RequireNetworkOff: profile.FloorFor(plan.Trust).RequiresNetworkOff(), TrustProfile: string(tr), Obs: obs,
		RequiredTrust: string(resolutionTrace.ProfileRequiredTrust), Canonical: resolutionTrace.Canonical, FieldProvenance: traceProvenance(resolutionTrace),
		MaxIterations: resolved.MaxIters, MaxTokens: resolved.MaxTokens, RunTimeout: resolved.RunTimeout, VerifyTimeout: *f.verifyTimeout,
		VerifyCmd: *f.verifyCmd, SkipVerifyBaseline: f.verifyBaseline.skip, AbortOnRedBaseline: f.verifyBaseline.abort,
		VerifyLastRun: *f.verifyLastRun, VerifyContinue: resolved.VerifyContinue, TestFence: fence, DiffScope: scope, Reviewer: reviewer,
		ReviewPolicy: reviewPolicy, ReviewUnverified: *f.reviewUnverified, RequireDiff: resolved.RequireDiff, ReviewRounds: *f.reviewRounds,
		Planner: planner, ReasoningEffort: resolved.Effort, PromptProfile: *f.promptProfile, CodeAct: *f.codeAct, ReproFirst: *f.reproFirst,
		ReproGate: *f.reproGate, BatchReads: resolved.BatchReads, BootContext: resolved.BootContext, ChurnNudgeRuns: resolved.ChurnNudgeRuns,
		MaxWallClock: *f.maxWall, MaxTotalTokens: *f.maxTotalTokens, FinishNudgeWindow: *f.finishNudge,
		DiagnoseCmd: *f.diagnoseCmd, DiagnoseAfterEdits: *f.diagnoseAfterEdits,
		AutoVerify: resolved.AutoVerify, AutoVerifySoft: resolved.AutoVerifySoft, StandingContext: resolved.StandingContext,
		NavSpiralWindow: resolved.NavSpiralWindow, AnswerNudgeWindow: resolved.AnswerNudgeWindow,
	})
	if plan.GatePolicy != nil {
		baseCfg.ApprovalPolicyName = plan.GatePolicy.Name
		baseCfg.ApprovalPolicyHash = plan.GatePolicy.Hash()
	}
	wireBudgetControls(&baseCfg, *f.budget, modelLabel(model), extras.Price)
	baseCfg.AllowUnpricedSpend = *f.allowUnpricedSpend

	if ladderMode {
		fmt.Fprintln(errW, "ladder: ON")
		ladderRun := func(ctx context.Context, cfg agent.Config) (*agent.RunResult, error) {
			p := cfg.Model
			if p == nil {
				return nil, errors.New("ladder attempt has no provider")
			}
			if *f.protocol != "tools" || !p.Capabilities().Tools {
				return nil, fmt.Errorf("ladder slice 2 requires native tool-capable rungs; model %q lacks tool support or -protocol=text was set", modelLabel(p))
			}
			res, err := runSplit(ctx, agent.RunNative, cfg)
			if res != nil && *f.transcriptDir != "" {
				if path, werr := agent.WriteTranscript(*f.transcriptDir, agent.RecordFrom(res, modelLabel(p))); werr != nil {
					fmt.Fprintln(errW, "transcript:", werr)
				} else {
					fmt.Fprintln(errW, "transcript: "+path)
				}
			}
			return res, err
		}
		reviewerFactory := func(ref string) (agent.Reviewer, error) {
			rp, err := roleProvider(ref)
			if err != nil {
				return nil, err
			}
			if extras.NewReviewer == nil {
				return nil, errors.New("-review-model: not available in this build")
			}
			return extras.NewReviewer(rp, ref, *f.reviewEffort), nil
		}
		lr, lerr := extras.RunLadder(ctx, LadderRequest{Config: ladderCfg, Base: baseCfg, Dir: cwd, Run: ladderRun, Provider: roleProvider, Reviewer: reviewerFactory, Price: extras.Price, RecordDir: *f.transcriptDir, Task: taskVal})
		if lerr != nil {
			return failSetup(os.Stdout, errW, outFmt, "ladder", lerr.Error())
		}
		if lr == nil || lr.RunResult == nil {
			return failSetup(os.Stdout, errW, outFmt, "ladder", "ladder capability returned no result")
		}
		if lr != nil && lr.RecordErr != nil {
			fmt.Fprintf(errW, "ladder: WARNING — record write failed: %v (run result is unaffected)\n", lr.RecordErr)
		}
		res := lr.RunResult
		label := "ladder"
		if lr != nil && lr.WinnerModel != "" {
			label = lr.WinnerModel
		}
		exitCode := finalize(buildResultWithPrice(res, label, "", baselineCommit, baselineDirtyFiles, extras.Price, "", "", ""), nil)
		return exitCode
	}

	// The loop returns a non-nil RunResult on every path; a non-nil error is
	// redundant with Outcome==ProviderErr (and res.Err), so we drive everything off
	// the typed RunResult below and discard the error.
	// -report on a NON-worktree git run needs a pre-run snapshot so the report's
	// diff shows only this run's changes, not pre-existing dirt (worktree runs
	// get their diff from the banked patch instead). Best-effort: a snapshot
	// failure degrades to a report without a diff section, never a dead run.
	var delta *reportDelta
	if *f.report != "" && worktreeDir == "" && vcs.IsRepo(ctx, origCwd) {
		var derr error
		if delta, derr = captureReportDelta(origCwd); derr != nil {
			fmt.Fprintln(errW, "report: pre-run snapshot failed (report will omit the diff):", derr)
		}
	}

	// S6d.2s-b: the direct run path resolves through agent.Prepare — the flag
	// layer's own profile.Resolve no longer feeds the loop. Bindings + record
	// identity come from baseCfg (unaffected by the requested-side split); only
	// the POLICY is rebuilt natively so runspec.Resolve owns profile threading
	// and the memory/worktree record corrections land (PROFILES.md §7.5 S6d.2s).
	// baseCfg survives for the not-yet-migrated best-of and ladder paths.
	directReq := buildBaseRequest(baseRequestInput{
		TrustProfile: string(tr), ExecProfileName: execProfile.Name, Overrides: ov,
		VerifyTimeout: *f.verifyTimeout, VerifyCmd: *f.verifyCmd,
		SkipVerifyBaseline: f.verifyBaseline.skip, AbortOnRedBaseline: f.verifyBaseline.abort, VerifyLastRun: *f.verifyLastRun,
		ReviewPolicy: reviewPolicy, ReviewUnverified: *f.reviewUnverified, ReviewRounds: *f.reviewRounds,
		TestFence: fence, DiffScope: scope, PromptProfile: *f.promptProfile,
		CodeAct: *f.codeAct, ReproFirst: *f.reproFirst, ReproGate: *f.reproGate,
		MaxWallClock: *f.maxWall, MaxTotalTokens: *f.maxTotalTokens, MaxTotalCostUSD: *f.budget, AllowUnpricedSpend: *f.allowUnpricedSpend,
		DiagnoseCmd: *f.diagnoseCmd, DiagnoseAfterEdits: *f.diagnoseAfterEdits, SolverModel: modelLabel(model),
	})
	directRT, directContent := baseCfg.Bindings()
	directPrepared, prepErr := agent.Prepare(directReq, directRT, directContent, agent.RecordInputs{
		BinaryIdentity: baseCfg.BinaryIdentity, InvocationSurface: invocationSurface,
		RequestedProtocol: *f.protocol, ProtocolFallbackReason: fallbackReason, CLIOverrides: cliOverrides(fs),
		ApprovalPolicyName: baseCfg.ApprovalPolicyName, ApprovalPolicyHash: baseCfg.ApprovalPolicyHash,
	})
	if prepErr != nil {
		return failSetup(os.Stdout, errW, outFmt, "config", prepErr.Error())
	}
	res, runErr := loopFn(ctx, directPrepared.Spec(), directPrepared.Runtime(), directPrepared.Content())
	if runErr != nil {
		var serr *agent.SetupError
		if errors.As(runErr, &serr) {
			kind := serr.Kind
			if kind == "" {
				kind = "setup"
			}
			return failSetup(os.Stdout, errW, outFmt, kind, serr.Error())
		}
	}
	if res == nil {
		// The loop contract returns a non-nil RunResult on every path; guard anyway
		// so we never silently emit nothing on the data channel.
		return failSetup(os.Stdout, errW, outFmt, "internal", "agent returned no result")
	}
	// The memory store (if any) runs in the background so the result is emitted
	// without waiting on it (review #4) — but it must land before the PROCESS
	// exits. Deferred, so it settles after emit/review, right before we return.
	defer res.AwaitMemory()

	label := ""
	if mp, ok := model.(interface{ Model() string }); ok {
		label = mp.Model()
	}

	// Persist the full run by default (P1 spine): the trace is no longer lost when
	// the process exits, and every run is addressable by res.ID. Best-effort — a
	// write failure yields transcript_path:null in the result and a stderr warning,
	// but NEVER changes the exit code (D3/O7): a good answer must not become a
	// failure because the disk was full. -transcript-dir="" opts out (→ null).
	transcriptPath := ""
	if *f.transcriptDir != "" {
		if path, werr := agent.WriteTranscript(*f.transcriptDir, agent.RecordFrom(res, label)); werr != nil {
			fmt.Fprintln(errW, "transcript:", werr)
		} else {
			transcriptPath = path
			fmt.Fprintln(errW, "transcript: "+path)
		}
	}

	worktreePath := ""
	patchPath := ""
	patchError := ""
	var worktreeForReviewOnly bool
	if worktreeDir != "" {
		patchDir := ""
		switch {
		case transcriptPath != "":
			patchDir = filepath.Dir(transcriptPath)
		case *f.transcriptDir != "":
			patchDir = *f.transcriptDir
		default:
			fmt.Fprintln(errW, "worktree: transcript disabled; cannot write patch next to transcript, keeping worktree for inspection: "+worktreeDir)
			worktreePath = worktreeDir
			worktreeHandled = true
		}
		if patchDir != "" {
			candidatePatch := filepath.Join(patchDir, res.ID+".patch")
			changed, cerr := worktreeCollect(worktreeDir, candidatePatch)
			if cerr != nil {
				patchError = collectPatchErrorMessage(worktreeDir, cerr)
				fmt.Fprintln(errW, "worktree: collecting patch failed (forcing exit 1; keeping worktree if it still exists):", patchError)
				worktreePath = worktreeDir
				worktreeHandled = true
				if changed {
					// The worktree has changes, but the patch did not land; keep
					// patchPath empty so JSON reports patch_path:null.
				}
			} else if changed {
				patchPath = candidatePatch
				worktreeHandled = true
				if shouldRemovePatchedWorktree(res.Outcome, *f.review, *f.keepWorktree) {
					if rerr := worktreeRemove(worktreeDir); rerr != nil {
						fmt.Fprintln(errW, "worktree: patch written, but cleanup failed (keeping worktree; run exit unchanged):", rerr)
						worktreePath = worktreeDir
					} else {
						fmt.Fprintf(errW, "worktree: changes captured in patch: %s; removed worktree %s\n", patchPath, worktreeDir)
					}
				} else {
					worktreePath = worktreeDir
					fmt.Fprintf(errW, "worktree: changes kept at %s; patch: %s\n", worktreeDir, patchPath)
				}
			} else if *f.review && res.Outcome != agent.RefusedUnsafe {
				// Clean worktree, but we are about to run reviewChanges(ctx, cwd, ...).
				// If we remove the worktree now, cwd (which points to worktreeDir)
				// will be invalid, and git commands in reviewChanges will fail.
				// Keep it for now; we'll try to remove it after review.
				worktreePath = worktreeDir
				worktreeHandled = true
				worktreeForReviewOnly = true
				fmt.Fprintln(errW, "worktree: clean; keeping for review at "+worktreeDir)
			} else if rerr := worktreeRemove(worktreeDir); rerr != nil {
				fmt.Fprintln(errW, "worktree: clean remove failed (keeping worktree; run exit unchanged):", rerr)
				worktreePath = worktreeDir
				worktreeHandled = true
			} else {
				worktreeHandled = true
				fmt.Fprintln(errW, "worktree: clean; removed "+worktreeDir)
			}
		}
	}

	// Emit the result on stdout (D1 data channel) for EVERY outcome — including
	// errors, fixing the old bug where the SUMMARY printed only on err==nil. The
	// returned exit code carries the outcome class so a script can branch on $? (D4).
	exitCode := finalize(buildResultWithPrice(res, label, transcriptPath, baselineCommit, baselineDirtyFiles, extras.Price, worktreePath, patchPath, patchError), delta)

	// Review the agent's changes before exiting — regardless of outcome, so a
	// failed or stuck run's partial edits can be discarded rather than silently
	// left in the tree. Skip on RefusedUnsafe: the run was refused before the first
	// model call, so by construction there are no changes to review. Interactive for
	// now (non-interactive review policies are D5); a git-infra failure here is a
	// genuine exit-1 setup error that overrides the run's own code.
	if *f.review && res.Outcome != agent.RefusedUnsafe {
		if rerr := reviewChanges(ctx, cwd, commitMessage(taskVal), *f.reviewAction, tty); rerr != nil {
			fmt.Fprintln(errW, "review:", rerr)
			return 1
		}
		// If we kept a clean worktree specifically for this review step, and it
		// is still clean (e.g. the user didn't 'keep' it or make manual changes
		// that they now want to save), try to remove it now.
		if worktreeForReviewOnly {
			if changed, cerr := worktreeCollect(worktreeDir, ""); cerr == nil && !changed {
				if rerr := worktreeRemove(worktreeDir); rerr != nil {
					fmt.Fprintln(errW, "worktree: post-review remove failed (keeping worktree; run exit unchanged):", rerr)
				} else {
					fmt.Fprintln(errW, "worktree: clean; removed "+worktreeDir+" after review")
				}
			}
		}
	}
	return exitCode
}

// shouldRemovePatchedWorktree decides whether a changed -worktree run should be
// collapsed to its banked patch instead of leaving a registered detached worktree.
func shouldRemovePatchedWorktree(outcome agent.Outcome, review, keepWorktree bool) bool {
	// Once changes are banked in <run-id>.patch, the patch is the deliverable; a
	// lingering /tmp/driver-agent-wt-* checkout is mostly a cleanup footgun and can
	// invite broad external sweeps that destroy live concurrent runs. Keep it only
	// when -review still needs the live tree for the end-of-run diff/commit flow, or
	// when the operator explicitly asks for inspection with -keep-worktree.
	_ = outcome // Kept in the signature so table tests document outcome-independence.
	return !review && !keepWorktree
}

func collectPatchErrorMessage(worktreeDir string, err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) || (strings.Contains(lower, "chdir") && strings.Contains(lower, "no such file or directory")) {
		return fmt.Sprintf("worktree directory vanished mid-run: %s (likely external interference, e.g. a concurrent worktree sweep); the run's work is lost: %v", worktreeDir, err)
	}
	return msg
}

// validateReviewFlags rejects unknown -approve / -review-action values up front, so
// a typo can't silently weaken the gate (e.g. an unrecognized -approve falling
// through to a permissive default). The values only take effect under -review, but
// validating them unconditionally catches the typo regardless.
func validateReviewFlags(approve, reviewAction string) error {
	switch approve {
	case "interactive", "policy", "never":
	default:
		return fmt.Errorf("unknown -approve %q (want 'interactive', 'policy', or 'never')", approve)
	}
	switch reviewAction {
	case "prompt", "commit", "discard", "keep":
	default:
		return fmt.Errorf("unknown -review-action %q (want 'prompt', 'commit', 'discard', or 'keep')", reviewAction)
	}
	return nil
}

// verifyBaselineValue is a flag.Value for -verify-baseline that accepts
// 'on' (default warn), 'off' (skip), 'abort' (refuse if red),
// 'true'/'false' (aliases for on/off). It implements IsBoolFlag so the
// bare form -verify-baseline works (Set("true") → on).
type verifyBaselineValue struct {
	skip  bool
	abort bool
}

func (v *verifyBaselineValue) String() string {
	if v.abort {
		return "abort"
	}
	if v.skip {
		return "off"
	}
	return "on"
}

func (v *verifyBaselineValue) Set(s string) error {
	switch s {
	case "on", "true":
		v.skip = false
		v.abort = false
	case "off", "false":
		v.skip = true
		v.abort = false
	case "abort":
		v.skip = false
		v.abort = true
	default:
		return fmt.Errorf("invalid -verify-baseline value %q: must be 'on' (default), 'off', 'abort', 'true', or 'false'", s)
	}
	return nil
}

func (v *verifyBaselineValue) IsBoolFlag() bool { return true }

type bestOfCLIOptions struct {
	N                 int
	OrigCwd           string
	Task              string
	Model             llm.Provider
	ModelLabel        string
	InvocationSurface string
	Selector          bestOfSelector
	SelectorModel     string
	// Overrides are the profile-covered flags the operator explicitly set
	// (fs.Visit presence) — threaded so buildAgentConfigInDir resolves through
	// agent.Prepare with the exec profile forwarded (S6d.2s-b). Distinct from
	// the resolved MaxIters/etc. below, which the legacy Config path used.
	Overrides             profile.Overrides
	TrustPlan             trustPlan
	Session               bool
	Review                bool
	KeepWorktree          bool
	Approve, ReviewAction string
	ReviewRounds          int
	ReviewRequired        bool
	ReviewFailOpen        bool
	ReviewOptional        bool
	ReviewUnverified      bool
	RequireDiff           bool
	Reviewer              agent.Reviewer
	Planner               agent.Planner
	Memory                bool
	SkillsFlag, Protocol  string
	OutFmt                outputFormat
	// Finalize is runMain's shared result sink (role-model stamp + ledger +
	// -report). Nil-safe: when unset the result is emitted directly.
	Finalize                       func(resultObject, *reportDelta) int
	TranscriptDir                  string
	MaxIters, MaxTokens            int
	RunTimeout                     time.Duration
	VerifyTimeout                  time.Duration
	VerifyCmd                      string
	SkipVerifyBaseline             bool
	AbortOnRedBaseline             bool
	VerifyLastRun, VerifyContinue  bool
	TestFence                      []string
	DiffScope                      []string
	ReasoningEffort, PromptProfile string
	CodeAct                        bool
	ReproFirst                     bool
	ReproGate                      bool
	BatchReads                     bool
	BootContext                    bool
	ChurnNudge, FinishNudge        int
	DiagnoseCmd                    string
	DiagnoseAfterEdits             int
	MaxWall                        time.Duration
	MaxTotalTokens                 int
	MaxTotalCostUSD                float64
	AllowUnpricedSpend             bool
	Price                          PriceLookup
	OpenMemory                     func(string) (memory.Store, error)
	MemoryDBName                   string
	ReadWindow                     int
	ReadOutline                    bool
	ExecProfileName                string
	ExecProfileHash                string
	CLIOverrides                   []string
	RequiredTrust                  string
	Canonical                      bool
	FieldProvenance                map[string]string
	AutoVerify, AutoVerifySoft     bool
	StandingContext                bool
	NavSpiralWindow                int
	AnswerNudgeWindow              int
}

func modelLabel(p llm.Provider) string {
	if mp, ok := p.(interface{ Model() string }); ok {
		return mp.Model()
	}
	return ""
}

func runBestOfCLI(ctx context.Context, opts bestOfCLIOptions) int {
	fmt.Fprintf(errW, "best-of: ON — n=%d selector=%s\n", opts.N, opts.SelectorModel)
	var tty *ttyIO
	if opts.Review && (opts.Approve == "interactive" || opts.ReviewAction == "prompt") {
		t, terr := openTTY()
		if terr != nil {
			return failSetup(os.Stdout, errW, opts.OutFmt, "no_tty", "review needs an interactive terminal for -approve=interactive / -review-action=prompt, but /dev/tty could not be opened ("+terr.Error()+") — run non-interactively with -approve=policy (or =never) and -review-action=commit|discard|keep")
		}
		tty = t
		defer tty.Close()
	}

	attempts := make([]attemptRecord, 0, opts.N)
	patches := make([]string, 0, opts.N)
	results := make([]*agent.RunResult, 0, opts.N)
	worktrees := make([]string, 0, opts.N)
	var firstBaselineCommit string
	var firstBaselineDirtyFiles []string

	for i := 1; i <= opts.N; i++ {
		wi, err := worktreeAdd(opts.OrigCwd)
		if err != nil {
			return failSetup(os.Stdout, errW, opts.OutFmt, "worktree", "best-of attempt worktree: "+err.Error())
		}
		worktrees = append(worktrees, wi.Dir)
		if i == 1 {
			firstBaselineCommit = wi.BaseCommit
			firstBaselineDirtyFiles = wi.DirtyFiles
		}
		if len(wi.DirtyFiles) > 0 {
			show := wi.DirtyFiles
			truncated := ""
			if len(show) > 20 {
				show = show[:20]
				truncated = "…"
			}
			fmt.Fprintf(errW, "best-of: attempt %d/%d worktree: %s (baseline %s = dirty-tree snapshot of the main checkout; %d files differ from HEAD: %s%s)\n",
				i, opts.N, wi.Dir, wi.BaseCommit, len(wi.DirtyFiles), strings.Join(show, ", "), truncated)
		} else {
			fmt.Fprintf(errW, "best-of: attempt %d/%d worktree: %s (baseline %s = HEAD; main checkout was clean)\n",
				i, opts.N, wi.Dir, wi.BaseCommit)
		}
		attemptOpts := bestOfAttemptOptions(opts)
		res := runSolveInDir(ctx, attemptOpts, wi.Dir, tty)
		if res == nil {
			return failSetup(os.Stdout, errW, opts.OutFmt, "internal", "agent returned no result")
		}
		defer res.AwaitMemory()
		patch, perr := diffWorktree(ctx, wi.Dir)
		if perr != nil {
			fmt.Fprintln(errW, "best-of: collecting attempt diff failed:", perr)
		}
		costPtr := (*float64)(nil)
		if opts.Price != nil {
			if cost, ok := opts.Price(opts.ModelLabel, res.Usage); ok {
				costPtr = &cost
			}
		}
		var ans *string
		if res.Outcome == agent.Answered || res.Outcome == agent.Unverified {
			a := res.Answer
			ans = &a
		}
		// attempts record raw solve outcome — intentional: the top-level result may
		// reflect a post-review downgrade while attempts stay honest pre-review.
		attempts = append(attempts, attemptRecord{Index: i, Outcome: res.Outcome, ExitCode: exitCodeFor(res.Outcome), PatchNonEmpty: strings.TrimSpace(patch) != "", Answer: ans, Cost: costPtr, Usage: res.Usage})
		patches = append(patches, patch)
		results = append(results, res)
	}

	winner, report := selectBestOf(ctx, attempts, patches, opts.Task, opts.Selector, opts.SelectorModel)
	priceBestOfReport(&report, opts.Price)
	if winner < 0 || winner >= len(results) {
		winner = len(results) - 1
	}
	res := results[winner]
	winnerWT := worktrees[winner]
	fmt.Fprintf(errW, "best-of: selected attempt %d/%d\n", winner+1, opts.N)
	for i, wt := range worktrees {
		if i == winner {
			continue
		}
		if err := worktreeRemove(wt); err != nil {
			fmt.Fprintln(errW, "best-of: losing worktree cleanup failed:", err)
		}
	}
	if opts.Reviewer != nil && res.Outcome != agent.RefusedUnsafe {
		baseTree, berr := worktreeHeadTree(ctx, winnerWT)
		if berr != nil {
			fmt.Fprintln(errW, "best-of: winner review baseline unavailable:", berr)
		} else {
			fmt.Fprintln(errW, "best-of: running review gate on selected winner only")
			reviewed, rerr := reviewBestOfWinner(ctx, opts, winnerWT, baseTree, res, tty)
			if rerr != nil {
				fmt.Fprintln(errW, "best-of: winner review failed:", rerr)
				return 1
			}
			if reviewed != nil {
				res = reviewed
				results[winner] = reviewed
			}
			if patch, perr := diffWorktree(ctx, winnerWT); perr != nil {
				fmt.Fprintln(errW, "best-of: collecting reviewed winner diff failed:", perr)
			} else {
				patches[winner] = patch
			}
		}
	}

	transcriptPath := ""
	if opts.TranscriptDir != "" {
		if path, werr := agent.WriteTranscript(opts.TranscriptDir, agent.RecordFrom(res, opts.ModelLabel)); werr != nil {
			fmt.Fprintln(errW, "transcript:", werr)
		} else {
			transcriptPath = path
			fmt.Fprintln(errW, "transcript: "+path)
		}
	}

	worktreePath, patchPath := "", ""
	patchError := ""
	worktreeForReviewOnly := false
	patchDir := ""
	if transcriptPath != "" {
		patchDir = filepath.Dir(transcriptPath)
	} else if opts.TranscriptDir != "" {
		patchDir = opts.TranscriptDir
	}
	if patchDir != "" {
		candidatePatch := filepath.Join(patchDir, res.ID+".patch")
		changed, cerr := worktreeCollect(winnerWT, candidatePatch)
		if cerr != nil {
			patchError = collectPatchErrorMessage(winnerWT, cerr)
			fmt.Fprintln(errW, "worktree: collecting patch failed (forcing exit 1; keeping worktree if it still exists):", patchError)
			worktreePath = winnerWT
		} else if changed {
			patchPath = candidatePatch
			if shouldRemovePatchedWorktree(res.Outcome, opts.Review, opts.KeepWorktree) {
				if rerr := worktreeRemove(winnerWT); rerr != nil {
					fmt.Fprintln(errW, "worktree: patch written, but cleanup failed (keeping worktree; run exit unchanged):", rerr)
					worktreePath = winnerWT
				} else {
					fmt.Fprintf(errW, "worktree: changes captured in patch: %s; removed worktree %s\n", patchPath, winnerWT)
				}
			} else {
				worktreePath = winnerWT
				fmt.Fprintf(errW, "worktree: changes kept at %s; patch: %s\n", winnerWT, patchPath)
			}
		} else if opts.Review && res.Outcome != agent.RefusedUnsafe {
			worktreePath = winnerWT
			worktreeForReviewOnly = true
			fmt.Fprintln(errW, "worktree: clean; keeping for review at "+winnerWT)
		} else if rerr := worktreeRemove(winnerWT); rerr != nil {
			fmt.Fprintln(errW, "worktree: clean remove failed (keeping worktree; run exit unchanged):", rerr)
			worktreePath = winnerWT
		} else {
			fmt.Fprintln(errW, "worktree: clean; removed "+winnerWT)
		}
	} else {
		fmt.Fprintln(errW, "worktree: transcript disabled; cannot write patch next to transcript, keeping worktree for inspection: "+winnerWT)
		worktreePath = winnerWT
	}

	bestOfResult := buildResultWithBestOfPrice(res, opts.ModelLabel, transcriptPath, &report, firstBaselineCommit, firstBaselineDirtyFiles, opts.Price, worktreePath, patchPath, patchError)
	var exitCode int
	if opts.Finalize != nil {
		exitCode = opts.Finalize(bestOfResult, nil)
	} else {
		exitCode = emitResult(os.Stdout, opts.OutFmt, bestOfResult)
	}
	if opts.Review && res.Outcome != agent.RefusedUnsafe {
		if rerr := reviewChanges(ctx, winnerWT, commitMessage(opts.Task), opts.ReviewAction, tty); rerr != nil {
			fmt.Fprintln(errW, "review:", rerr)
			return 1
		}
		if worktreeForReviewOnly {
			if changed, cerr := worktreeCollect(winnerWT, ""); cerr == nil && !changed {
				if rerr := worktreeRemove(winnerWT); rerr != nil {
					fmt.Fprintln(errW, "worktree: post-review remove failed (keeping worktree; run exit unchanged):", rerr)
				} else {
					fmt.Fprintln(errW, "worktree: clean; removed "+winnerWT+" after review")
				}
			}
		}
	}
	return exitCode
}

func buildAgentConfigInDir(ctx context.Context, opts bestOfCLIOptions, cwd string, tty *ttyIO) (agent.Prepared, agent.LoopFunc, func(), *agent.RunResult) {
	sb, err := cli.BuildSandboxWithEnvAllowlist(ctx, opts.TrustPlan.SandboxKind, cwd, opts.TrustPlan.Runtime, opts.TrustPlan.Network, opts.TrustPlan.EnvAllowlist)
	if err != nil {
		res := &agent.RunResult{Task: opts.Task, Root: cwd, Outcome: agent.ProviderErr, Reason: "sandbox: " + err.Error(), Err: err, StartedAt: time.Now(), EndedAt: time.Now()}
		return agent.Prepared{}, nil, func() {}, res
	}
	cleanup := func() { _ = sb.Close() }
	caps := sb.Capabilities()
	fmt.Fprintf(errW, "sandbox: %s (isolation=%s, network=%t) — %s\n", opts.TrustPlan.SandboxKind, caps.Isolation, caps.Network, cli.TrustLabel(caps.Isolation))
	minIsolation := opts.TrustPlan.MinIsolation
	toolBase := sb
	if opts.Session {
		if sr, ok := sb.(sandbox.Sessioner); ok {
			if sess, err := sr.NewSession(ctx); err == nil {
				prevCleanup := cleanup
				cleanup = func() { _ = sess.Close(); prevCleanup() }
				toolBase = sessionSandbox{Sandbox: sb, sess: sess}
			}
		}
	}
	gate := func(inner sandbox.Sandbox) sandbox.Sandbox {
		if opts.TrustPlan.GatePolicy != nil {
			return gated.New(inner, nil, approvalPolicyAdapter(*opts.TrustPlan.GatePolicy))
		}
		return inner
	}
	if opts.Review {
		approver, policy := reviewGate(opts.Approve, tty)
		trustGate := gate
		gate = func(inner sandbox.Sandbox) sandbox.Sandbox { return gated.New(trustGate(inner), approver, policy) }
	}
	effects := gate(toolBase)
	var verifyEffects sandbox.Sandbox
	if opts.Session {
		verifyEffects = gate(sb)
	}
	var tools map[string]agent.Tool
	if opts.SkillsFlag != "none" {
		projectDir := skill.ProjectDir(cwd)
		if !opts.TrustPlan.AllowProjectSkills {
			projectDir = ""
		}
		skills, warns := skill.Discover(skill.Sources{UserDir: skill.UserDir(), ProjectDir: projectDir, Explicit: cli.SplitList(opts.SkillsFlag)})
		for _, w := range warns {
			fmt.Fprintln(errW, "skills:", w)
		}
		if len(skills) > 0 {
			tools = agent.DefaultTools(effects, opts.RunTimeout, agent.ReadOptions{Window: opts.ReadWindow, Outline: opts.ReadOutline})
			tools["skill"] = skill.Tool(skills, sb, 0)
		}
	}
	var mem memory.Store
	if opts.Memory && opts.OpenMemory != nil {
		m, _ := opts.OpenMemory(filepath.Join(opts.OrigCwd, opts.MemoryDBName))
		mem = m
		if mem != nil {
			prevCleanup := cleanup
			cleanup = func() { _ = mem.Close(); prevCleanup() }
		}
	}
	loopFn := agent.Run
	fallbackReason := ""
	if opts.Protocol == "tools" && opts.Model.Capabilities().Tools {
		loopFn = agent.RunNative
	} else if opts.Protocol == "tools" {
		fallbackReason = "provider lacks tool capability"
	}
	obs := agent.NewWriterObserver(errW)
	if opts.OutFmt == formatNDJSON {
		obs = ndjsonObserver{os.Stdout}
	}
	if tools == nil {
		tools = agent.DefaultTools(effects, opts.RunTimeout, agent.ReadOptions{Window: opts.ReadWindow, Outline: opts.ReadOutline})
	}
	reviewPolicy, _ := agent.ReviewPolicyFrom(opts.ReviewRequired, opts.ReviewFailOpen, opts.ReviewOptional)
	cfg := agent.Config{
		BinaryIdentity:         agent.BinaryIdentityDriver,
		InvocationSurface:      opts.InvocationSurface,
		RequestedProtocol:      opts.Protocol,
		ProtocolFallbackReason: fallbackReason,
		ExecProfileName:        opts.ExecProfileName,
		ExecProfileHash:        opts.ExecProfileHash,
		CLIOverrides:           append([]string(nil), opts.CLIOverrides...),
		RequiredTrust:          opts.RequiredTrust,
		Canonical:              opts.Canonical,
		FieldProvenance:        opts.FieldProvenance,
		Model:                  opts.Model,
		ModelInfo:              llm.Lookup(opts.ModelLabel),
		Sandbox:                effects,
		VerifySandbox:          verifyEffects,
		Memory:                 mem,
		Tools:                  tools,
		Task:                   opts.Task,
		Root:                   cwd,
		MinIsolation:           minIsolation,
		RequireNetworkOff:      profile.FloorFor(opts.TrustPlan.Trust).RequiresNetworkOff(),
		TrustProfile:           string(opts.TrustPlan.Trust),
		Obs:                    obs,
		MaxIterations:          opts.MaxIters,
		MaxTokens:              opts.MaxTokens,
		RunTimeout:             opts.RunTimeout,
		VerifyTimeout:          opts.VerifyTimeout,
		VerifyCmd:              opts.VerifyCmd,
		SkipVerifyBaseline:     opts.SkipVerifyBaseline,
		AbortOnRedBaseline:     opts.AbortOnRedBaseline,
		VerifyLastRun:          opts.VerifyLastRun,
		VerifyContinue:         opts.VerifyContinue,
		TestFence:              opts.TestFence,
		DiffScope:              opts.DiffScope,
		Reviewer:               opts.Reviewer,
		ReviewPolicy:           reviewPolicy,
		ReviewUnverified:       opts.ReviewUnverified,
		RequireDiff:            opts.RequireDiff,
		ReviewRounds:           opts.ReviewRounds,
		Planner:                opts.Planner,
		ReasoningEffort:        opts.ReasoningEffort,
		PromptProfile:          opts.PromptProfile,
		CodeAct:                opts.CodeAct,
		ReproFirst:             opts.ReproFirst,
		ReproGate:              opts.ReproGate,
		BatchReads:             opts.BatchReads,
		BootContext:            opts.BootContext,
		ChurnNudgeRuns:         opts.ChurnNudge,
		MaxWallClock:           opts.MaxWall,
		MaxTotalTokens:         opts.MaxTotalTokens,
		FinishNudgeWindow:      opts.FinishNudge,
		DiagnoseCmd:            opts.DiagnoseCmd,
		DiagnoseAfterEdits:     opts.DiagnoseAfterEdits,
		AutoVerify:             opts.AutoVerify,
		AutoVerifySoft:         opts.AutoVerifySoft,
		StandingContext:        opts.StandingContext,
		TerminationPolicy:      agent.TerminationPolicy{NavSpiralWindow: opts.NavSpiralWindow},
		AnswerNudgeWindow:      opts.AnswerNudgeWindow,
	}
	if opts.TrustPlan.GatePolicy != nil {
		cfg.ApprovalPolicyName = opts.TrustPlan.GatePolicy.Name
		cfg.ApprovalPolicyHash = opts.TrustPlan.GatePolicy.Hash()
	}
	wireBudgetControls(&cfg, opts.MaxTotalCostUSD, opts.ModelLabel, opts.Price)
	cfg.AllowUnpricedSpend = opts.AllowUnpricedSpend

	// S6d.2s-b: resolve through agent.Prepare so the exec profile is forwarded
	// and the memory/worktree/read-window record corrections land. Bindings +
	// record identity come from cfg (unaffected by the requested-side split);
	// only the POLICY is rebuilt natively. cfg's resolved policy fields are dead
	// here (Prepare rebuilds them from the request) and vanish with Config at
	// S6d.7.
	req := buildBaseRequest(baseRequestInput{
		TrustProfile: string(opts.TrustPlan.Trust), ExecProfileName: opts.ExecProfileName, Overrides: opts.Overrides,
		VerifyTimeout: opts.VerifyTimeout, VerifyCmd: opts.VerifyCmd,
		SkipVerifyBaseline: opts.SkipVerifyBaseline, AbortOnRedBaseline: opts.AbortOnRedBaseline, VerifyLastRun: opts.VerifyLastRun,
		ReviewPolicy: reviewPolicy, ReviewUnverified: opts.ReviewUnverified, ReviewRounds: opts.ReviewRounds,
		TestFence: opts.TestFence, DiffScope: opts.DiffScope, PromptProfile: opts.PromptProfile,
		CodeAct: opts.CodeAct, ReproFirst: opts.ReproFirst, ReproGate: opts.ReproGate,
		MaxWallClock: opts.MaxWall, MaxTotalTokens: opts.MaxTotalTokens, MaxTotalCostUSD: opts.MaxTotalCostUSD, AllowUnpricedSpend: opts.AllowUnpricedSpend,
		DiagnoseCmd: opts.DiagnoseCmd, DiagnoseAfterEdits: opts.DiagnoseAfterEdits, SolverModel: opts.ModelLabel,
	})
	rt, content := cfg.Bindings()
	prepared, perr := agent.Prepare(req, rt, content, agent.RecordInputs{
		BinaryIdentity: cfg.BinaryIdentity, InvocationSurface: opts.InvocationSurface,
		RequestedProtocol: opts.Protocol, ProtocolFallbackReason: fallbackReason, CLIOverrides: opts.CLIOverrides,
		ApprovalPolicyName: cfg.ApprovalPolicyName, ApprovalPolicyHash: cfg.ApprovalPolicyHash,
	})
	if perr != nil {
		res := &agent.RunResult{Task: opts.Task, Root: cwd, Outcome: agent.ProviderErr, Reason: "config: " + perr.Error(), Err: perr, StartedAt: time.Now(), EndedAt: time.Now()}
		return agent.Prepared{}, nil, cleanup, res
	}
	return prepared, loopFn, cleanup, nil
}

func runSolveInDir(ctx context.Context, opts bestOfCLIOptions, cwd string, tty *ttyIO) *agent.RunResult {
	prepared, loopFn, cleanup, early := buildAgentConfigInDir(ctx, opts, cwd, tty)
	defer cleanup()
	if early != nil {
		return early
	}
	res, _ := loopFn(ctx, prepared.Spec(), prepared.Runtime(), prepared.Content())
	return res
}

func bestOfAttemptOptions(opts bestOfCLIOptions) bestOfCLIOptions {
	attemptOpts := opts
	attemptOpts.Reviewer = nil
	return attemptOpts
}

func reviewBestOfWinner(ctx context.Context, opts bestOfCLIOptions, cwd string, baseTree string, res *agent.RunResult, tty *ttyIO) (*agent.RunResult, error) {
	if opts.Reviewer == nil || res == nil || res.Outcome == agent.RefusedUnsafe {
		return res, nil
	}
	prepared, loopFn, cleanup, early := buildAgentConfigInDir(ctx, opts, cwd, tty)
	defer cleanup()
	if early != nil {
		return early, nil
	}
	return agent.ReviewAndRepairExistingWorkspace(ctx, prepared.Spec(), prepared.Runtime(), prepared.Content(), res, agent.ReviewPassOptions{BaseTree: baseTree}, loopFn)
}

// runSplit is the headless requested-side conversion: the assembled Config is
// resolved through runspec exactly once and the loop receives the split
// (PROFILES.md §7.5 — Config survives only as a requested-side input shape).
func runSplit(ctx context.Context, loop agent.LoopFunc, cfg agent.Config) (*agent.RunResult, error) {
	spec, rt, content, err := cfg.Split()
	if err != nil {
		return nil, err
	}
	return loop(ctx, spec, rt, content)
}

func worktreeHeadTree(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD^{tree}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD^{tree}: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func diffWorktree(ctx context.Context, dir string) (string, error) {
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "add", "-A", "-N").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add -N: %w: %s", err, strings.TrimSpace(string(out)))
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--binary", "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
