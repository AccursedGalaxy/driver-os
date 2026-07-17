package agent

// S6a test bridges: the agent tests were written against the flat-Config API.
// Rather than rewrite ~65 files in the same change that rewrites the loops,
// each bridge converts a Config template on the REQUESTED side (Config.Split —
// the sanctioned surviving shape, PROFILES.md §7.5) and calls the new API.
// Tests that specifically pin the S6b-deletable legacy fallbacks keep calling
// those functions directly. Deleted with Config in S6b.

import (
	"context"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/runspec"
)

// runT / runNativeT are old-signature loop bridges.
func runT(ctx context.Context, cfg Config) (*RunResult, error) {
	spec, rt, content, err := cfg.Split()
	if err != nil {
		return nil, err
	}
	return Run(ctx, spec, rt, content)
}

func runNativeT(ctx context.Context, cfg Config) (*RunResult, error) {
	spec, rt, content, err := cfg.Split()
	if err != nil {
		return nil, err
	}
	return RunNative(ctx, spec, rt, content)
}

// splitT resolves a Config template for tests that drive loop INTERNALS.
func splitT(tb testing.TB, cfg Config) (runspec.PolicyValue, Runtime, Content, *verifyState) {
	tb.Helper()
	spec, rt, content, err := cfg.Split()
	if err != nil {
		tb.Fatalf("splitT: resolve run spec: %v", err)
	}
	pol := spec.Policy()
	vs := runVerifyState(pol, rt)
	// Mirror the loop's own session-state wiring so gate tests observe the
	// session baseline cache exactly like a live run.
	return pol, rt, content, vs
}

// gateDepsT builds the gates dependency bundle from a Config template.
func gateDepsT(tb testing.TB, cfg Config) gateDeps {
	pol, rt, content, vs := splitT(tb, cfg)
	return gateDeps{
		pol:           pol,
		rt:            rt,
		vs:            vs,
		ev:            cfg.evidence,
		task:          content.Task,
		root:          content.Root,
		baselineCache: cfg.verifyBaselineCache,
	}
}

// newGatesT is the old-signature newGates bridge.
func newGatesT(ctx context.Context, tb testing.TB, cfg Config, runTimeout time.Duration) (*gates, error) {
	return newGates(ctx, gateDepsT(tb, cfg), runTimeout)
}

// resolveAutoVerifyT mimics the old mutate-the-Config semantics: it runs the
// new state-based resolution and writes the outcome back into cfg so existing
// assertions on cfg fields keep reading the values a live run would hold.
func resolveAutoVerifyT(ctx context.Context, tb testing.TB, cfg *Config) {
	pol, rt, content, vs := splitT(tb, *cfg)
	vs.resolved = cfg.autoVerifyResolved
	vs.provenance = cfg.autoVerifyProvenance
	resolveAutoVerify(ctx, vs, pol, rt, content.Root)
	cfg.VerifyCmd = vs.Cmd
	cfg.AutoVerify = vs.AutoVerify
	cfg.AutoVerifySoft = vs.AutoVerifySoft
	cfg.SkipVerifyBaseline = vs.SkipVerifyBaseline
	cfg.VerifyContinue = vs.VerifyContinue
	cfg.autoVerifyProvenance = vs.provenance
	cfg.autoVerifyResolved = vs.resolved
}

// verifyStateT builds a run-local verification state straight from a Config's
// verification fields (no resolution — for preamble/unit tests that assert on
// exact field combinations, including zero timeouts).
func verifyStateT(cfg Config) *verifyState {
	return &verifyState{
		Cmd:                cfg.VerifyCmd,
		Timeout:            cfg.VerifyTimeout,
		AutoVerify:         cfg.AutoVerify,
		AutoVerifySoft:     cfg.AutoVerifySoft,
		SkipVerifyBaseline: cfg.SkipVerifyBaseline,
		AbortOnRedBaseline: cfg.AbortOnRedBaseline,
		VerifyLastRun:      cfg.VerifyLastRun,
		VerifyContinue:     cfg.VerifyContinue,
		provenance:         cfg.autoVerifyProvenance,
		resolved:           cfg.autoVerifyResolved,
	}
}

// dollarBudgetStopT is the old-signature dollarBudgetStop bridge.
func dollarBudgetStopT(cfg Config, u llm.Usage, turns int, missingNoted *bool) (bool, string) {
	rt := Runtime{Spend: cfg.Spend, CostFn: cfg.CostFn, Obs: cfg.Obs}
	if rt.Obs == nil {
		rt.Obs = nopObserver{}
	}
	return dollarBudgetStop(cfg.MaxTotalCostUSD, cfg.AllowUnpricedSpend, rt, cfg.evidence, u, turns, missingNoted)
}

// newSessionT is the old-signature NewSession bridge.
func newSessionT(tb testing.TB, cfg Config, loop any) *Session {
	tb.Helper()
	s, err := NewSessionFromConfig(cfg, asLoopT(loop), nil)
	if err != nil {
		tb.Fatalf("newSessionT: %v", err)
	}
	return s
}

// resolveSystemPromptT is the old-signature resolveSystemPrompt bridge. It
// reads the prompt-affecting policy fields straight off the Config (no
// resolution — the fields are independent switches).
func resolveSystemPromptT(cfg Config) (prompt, note string, err error) {
	pol := runspec.PolicyValue{PromptProfile: cfg.PromptProfile, CodeAct: cfg.CodeAct, ReproFirst: cfg.ReproFirst, ReproGate: cfg.ReproGate, BatchReads: cfg.BatchReads}
	return resolveSystemPrompt(pol, cfg.Model)
}

// newTurnTrackerT is the old-signature newTurnTracker bridge.
func newTurnTrackerT(cfg Config, maxIter int, policy TerminationPolicy, counters *DetectorCounters) *turnTracker {
	deps := trackerDeps{
		verifyCmd:          cfg.VerifyCmd,
		churnNudgeRuns:     cfg.ChurnNudgeRuns,
		diagnoseCmd:        cfg.DiagnoseCmd,
		diagnoseAfterEdits: cfg.DiagnoseAfterEdits,
		finishNudgeWindow:  cfg.FinishNudgeWindow,
		verifySB:           cfg.verifySandbox(),
		obs:                cfg.Obs,
	}
	if deps.obs == nil {
		deps.obs = nopObserver{}
	}
	return newTurnTracker(deps, maxIter, policy, counters)
}

// seedMessagesT is the old-signature seedMessages bridge.
func seedMessagesT(cfg Config, env string) []llm.Message {
	return seedMessages(cfg.Task, Content{Task: cfg.Task, TaskImages: cfg.TaskImages, History: cfg.History, Root: cfg.Root}, env)
}

// verifyTerminationT is the old-signature verifyTermination bridge (exact
// verification fields off the Config, timeout via the legacy lazy rule).
func verifyTerminationT(ctx context.Context, cfg Config, lastRunFailed bool, runTimeout time.Duration) (reason, verifyOut string) {
	vs := verifyStateT(cfg)
	vs.Timeout = cfg.VerifyTimeout
	if vs.Timeout <= 0 { // the deleted lazy rule, preserved for old-shaped unit tests only.
		vs.Timeout = 5 * time.Minute
		if runTimeout > vs.Timeout {
			vs.Timeout = runTimeout
		}
	}
	rt := Runtime{Sandbox: cfg.Sandbox, VerifySandbox: cfg.VerifySandbox, Obs: cfg.Obs}
	if rt.Obs == nil {
		rt.Obs = nopObserver{}
	}
	return verifyTermination(ctx, vs, rt, cfg.evidence, lastRunFailed)
}

// wrapToolsT is the old-signature wrapTools bridge.
func wrapToolsT(cfg Config, runTimeout time.Duration) map[string]Tool {
	pol := runspec.PolicyValue{ReadWindow: cfg.ReadWindow, ReadOutline: cfg.ReadOutline, DiffScope: cfg.DiffScope, TestFence: cfg.TestFence, ReproFirst: cfg.ReproFirst}
	rt := Runtime{Tools: cfg.Tools, Sandbox: cfg.Sandbox}
	return wrapTools(pol, rt, cfg.Root, runTimeout)
}

// newFenceStateT is the old-signature newFenceState bridge.
func newFenceStateT(ctx context.Context, cfg Config) *fenceState {
	return newFenceState(ctx, cfg.TestFence, cfg.Sandbox, cfg.Obs)
}

// generateWithRetryT / generateWithEvictionT are old-signature bridges.
func generateWithRetryT(ctx context.Context, cfg Config, req llm.Request) (*llm.Response, error) {
	rt := Runtime{Model: cfg.Model, Obs: cfg.Obs}
	if rt.Obs == nil {
		rt.Obs = nopObserver{}
	}
	return generateWithRetry(ctx, rt, cfg.Stream, req)
}

func generateWithEvictionT(ctx context.Context, cfg Config, req llm.Request) (*llm.Response, []llm.Message, error) {
	rt := Runtime{Model: cfg.Model, Obs: cfg.Obs}
	if rt.Obs == nil {
		rt.Obs = nopObserver{}
	}
	return generateWithEviction(ctx, rt, cfg.Stream, req)
}

// legacyLoopT adapts an old-shape test loop stub into the new LoopFunc: the
// stub receives a Config VIEW reconstructed from the split (test-only; the
// production direction is forbidden and does not exist).
func legacyLoopT(fn func(context.Context, Config) (*RunResult, error)) LoopFunc {
	return func(ctx context.Context, spec runspec.ResolvedSpec, rt Runtime, content Content) (*RunResult, error) {
		pol := spec.Policy()
		cfg := Config{
			Model: rt.Model, Sandbox: rt.Sandbox, VerifySandbox: rt.VerifySandbox,
			Memory: rt.Memory, Tools: rt.Tools, Obs: rt.Obs, Reviewer: rt.Reviewer, Planner: rt.Planner,
			Spend: rt.Spend, CostFn: rt.CostFn, ModelInfo: rt.ModelInfo, PerfMark: rt.PerfMark,
			Task: content.Task, TaskImages: content.TaskImages, History: content.History, Root: content.Root,
			MaxIterations: pol.MaxIterations, MaxTokens: pol.MaxTokens, RunTimeout: pol.RunTimeout,
			VerifyTimeout: pol.VerifyTimeout, VerifyCmd: pol.VerifyCmd, VerifyContinue: pol.VerifyContinue,
			ReviewRounds: pol.ReviewRounds, ReviewPolicy: ReviewPolicy(pol.ReviewPolicy),
			ReasoningEffort: pol.ReasoningEffort, Persona: pol.Persona, Stream: pol.Stream,
			TestFence: pol.TestFence, DiffScope: pol.DiffScope, RequireDiff: pol.RequireDiff,
			TerminationPolicy:  pol.TerminationPolicy,
			AutoVerify:         pol.AutoVerify,
			AutoVerifySoft:     pol.AutoVerifySoft,
			SkipVerifyBaseline: pol.SkipVerifyBaseline,
		}
		// Reflect the Session's persisted verification state the way the real
		// loops consume it (runVerifyState), so old-shaped stubs that assert
		// on cfg verification fields observe what a live run would.
		if rt.sessionVerify != nil {
			vs := rt.sessionVerify
			cfg.VerifyCmd = vs.Cmd
			cfg.AutoVerify = vs.AutoVerify
			cfg.AutoVerifySoft = vs.AutoVerifySoft
			cfg.SkipVerifyBaseline = vs.SkipVerifyBaseline
			cfg.VerifyContinue = vs.VerifyContinue
			cfg.autoVerifyProvenance = vs.provenance
			cfg.autoVerifyResolved = vs.resolved
		}
		cfg.verifyBaselineCache = rt.verifyBaselineCache
		return fn(ctx, cfg)
	}
}

// reviewAndRepairT is the old-signature ReviewAndRepairExistingWorkspace bridge.
func reviewAndRepairT(ctx context.Context, tb testing.TB, cfg Config, base *RunResult, opts ReviewPassOptions, loop func(context.Context, Config) (*RunResult, error)) (*RunResult, error) {
	spec, rt, content, err := cfg.Split()
	if err != nil {
		tb.Fatalf("reviewAndRepairT: %v", err)
	}
	var lf LoopFunc
	if loop != nil {
		lf = legacyLoopT(loop)
	}
	return ReviewAndRepairExistingWorkspace(ctx, spec, rt, content, base, opts, lf)
}

// newGateT is the old-signature NewGate bridge.
func newGateT(ctx context.Context, tb testing.TB, cfg Config) (*Gate, error) {
	spec, rt, content, err := cfg.Split()
	if err != nil {
		tb.Fatalf("newGateT: %v", err)
	}
	return NewGate(ctx, spec, rt, content)
}

// must-style variants for call sites the mechanical rename produced (no
// testing.TB in scope there); resolution failure panics — a test template
// that fails Resolve is a broken test.
func mustSplitT(cfg Config) (runspec.ResolvedSpec, Runtime, Content) {
	spec, rt, content, err := cfg.Split()
	if err != nil {
		panic("bridge: resolve run spec: " + err.Error())
	}
	return spec, rt, content
}

// asLoopT accepts either loop shape (the bridges' callers pass both new-
// signature loops and old-shaped test stubs).
func asLoopT(loop any) LoopFunc {
	switch fn := loop.(type) {
	case nil:
		return nil
	case LoopFunc:
		return fn
	case func(context.Context, runspec.ResolvedSpec, Runtime, Content) (*RunResult, error):
		return fn
	case func(context.Context, Config) (*RunResult, error):
		return legacyLoopT(fn)
	default:
		panic("bridge: unsupported loop shape")
	}
}

func newSessionT2(cfg Config, loop any) *Session {
	s, err := NewSessionFromConfig(cfg, asLoopT(loop), nil)
	if err != nil {
		panic("bridge: " + err.Error())
	}
	return s
}

func newSessionWithT(cfg Config, loop any, history []llm.Message) *Session {
	s, err := NewSessionFromConfig(cfg, asLoopT(loop), history)
	if err != nil {
		panic("bridge: " + err.Error())
	}
	return s
}

func newGatesOld(ctx context.Context, cfg Config, runTimeout time.Duration) (*gates, error) {
	spec, rt, content := mustSplitT(cfg)
	pol := spec.Policy()
	vs := runVerifyState(pol, rt)
	return newGates(ctx, gateDeps{pol: pol, rt: rt, vs: vs, ev: cfg.evidence, task: content.Task, root: content.Root, baselineCache: cfg.verifyBaselineCache}, runTimeout)
}

func resolveAutoVerifyOld(ctx context.Context, cfg *Config) {
	spec, rt, content := mustSplitT(*cfg)
	pol := spec.Policy()
	vs := runVerifyState(pol, rt)
	vs.resolved = cfg.autoVerifyResolved
	vs.provenance = cfg.autoVerifyProvenance
	resolveAutoVerify(ctx, vs, pol, rt, content.Root)
	cfg.VerifyCmd = vs.Cmd
	cfg.AutoVerifySoft = vs.AutoVerifySoft
	cfg.SkipVerifyBaseline = vs.SkipVerifyBaseline
	cfg.VerifyContinue = vs.VerifyContinue
	cfg.autoVerifyProvenance = vs.provenance
	cfg.autoVerifyResolved = vs.resolved
}

func verifyGatePreambleT(cfg Config) string { return verifyGatePreamble(verifyStateT(cfg)) }

func effectiveReviewRequiredT(cfg Config) bool {
	v := int(cfg.ReviewPolicy)
	pol := runspec.PolicyValue{ReviewPolicy: v}
	return effectiveReviewRequired(gateDeps{pol: pol, rt: Runtime{Reviewer: cfg.Reviewer}})
}

func reviewExistingOld(ctx context.Context, cfg Config, runTimeout time.Duration, opts ReviewPassOptions) (string, string, *ReviewReport) {
	spec, rt, content := mustSplitT(cfg)
	return ReviewExistingWorkspace(ctx, spec, rt, content, runTimeout, opts)
}

func reviewAndRepairOld(ctx context.Context, cfg Config, base *RunResult, opts ReviewPassOptions, loop any) (*RunResult, error) {
	spec, rt, content := mustSplitT(cfg)
	return ReviewAndRepairExistingWorkspace(ctx, spec, rt, content, base, opts, asLoopT(loop))
}

func newGateOld(ctx context.Context, cfg Config) (*Gate, error) {
	spec, rt, content := mustSplitT(cfg)
	return NewGate(ctx, spec, rt, content)
}

// newConfigRecordT is the old-signature newConfigRecord bridge.
func newConfigRecordT(cfg Config, systemPrompt string, toolRepresentation any, protocols ...string) *ConfigRecord {
	spec, rt, _ := mustSplitT(cfg)
	pol := spec.Policy()
	return newConfigRecord(spec, rt, runVerifyState(pol, rt), systemPrompt, toolRepresentation, protocols...)
}

// wrapToolsRepro rebuilds the fenced toolset for a constructed gates value.
func wrapToolsRepro(g *gates, runTimeout time.Duration) map[string]Tool {
	return wrapTools(g.d.pol, g.d.rt, g.d.root, runTimeout)
}
