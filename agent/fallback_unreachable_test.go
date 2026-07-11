package agent

// S6a fallback-unreachability oracle (PROFILES.md §7.5): the legacy lazy
// default-filling helpers are still compiled but must be PROVEN DEAD before
// S6b deletes them. Two halves:
//
//  1. For ANY Resolve output, every legacy fallback is an identity function —
//     the `<= 0 → default` branches cannot fire on a complete spec. Shown by
//     round-tripping resolved values through each legacy helper across a
//     matrix of requested variants (the same variants the equivalence pin
//     uses, plus profile-driven and adversarial explicit-zero attempts, which
//     Resolve must reject rather than repair).
//  2. The loops ASSERT completeness rather than repair it: a zero ResolvedSpec
//     — the only non-Resolve value a caller can construct, since policy
//     storage is unexported — is refused at entry before any environmental
//     work, and the record path re-deriving from the same complete value is
//     byte-identical (effectiveConfigFromSpec vs the legacy lazy projection).
import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/internal/runspec"
)

func rsString(v string) *string { return &v }

// unreachabilityVariants is the requested-side matrix: base defaults, every
// knob explicitly set, profile-driven resolution, and partial termination
// policies whose unset fields the resolver (not the loop) must fill.
func unreachabilityVariants() map[string]runspec.RequestedConfig {
	full := runspec.TerminationPolicy{
		Version: "test", MaxRepeats: 7, MaxReasoningRepeats: 8, MaxStagnant: 9,
		NavSpiralWindow: 10, WanderMultiple: 11, FrontierCap: 12, GreenRepeatThreshold: 13,
	}
	return map[string]runspec.RequestedConfig{
		"defaults":           {},
		"explicit knobs":     {MaxIterations: rsInt(17), MaxTokens: rsInt(4096), RunTimeout: rsDuration(45 * time.Second), ReviewRounds: rsInt(4), VerifyTimeout: rsDuration(90 * time.Second)},
		"full policy":        {TerminationPolicy: rsPolicy(full)},
		"partial policy":     {TerminationPolicy: rsPolicy(runspec.TerminationPolicy{MaxRepeats: 7})},
		"alias only":         {NavSpiralWindow: rsInt(7)},
		"profile coding-v2":  {ExecProfileName: rsString("coding-v2")},
		"profile observe-v1": {ExecProfileName: rsString("observe-v1")},
		"profile + override": {ExecProfileName: rsString("coding-v2"), MaxIterations: rsInt(3), ReviewRounds: rsInt(1)},
	}
}

// configFromPolicy projects a resolved policy back onto the legacy Config
// shape so the legacy fallback helpers can be fed exactly what a loop holds.
func configFromPolicy(p runspec.PolicyValue) Config {
	return Config{
		MaxIterations: p.MaxIterations, MaxTokens: p.MaxTokens, RunTimeout: p.RunTimeout,
		VerifyTimeout: p.VerifyTimeout, ReviewRounds: p.ReviewRounds,
		TerminationPolicy: p.TerminationPolicy,
	}
}

// TestLegacyFallbacksUnreachableOnResolvedSpecs proves each S6b-deletable
// branch dead: fed a Resolve output, the legacy helper returns its input.
func TestLegacyFallbacksUnreachableOnResolvedSpecs(t *testing.T) {
	for name, req := range unreachabilityVariants() {
		t.Run(name, func(t *testing.T) {
			spec, _, err := runspec.Resolve(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := spec.Complete(); err != nil {
				t.Fatalf("Resolve output failed Complete(): %v", err)
			}
			p := spec.Policy()
			cfg := configFromPolicy(p)

			// resolveKnobs: every `<= 0 → default` branch must be a no-op.
			knobs := resolveKnobs(cfg)
			if knobs.maxIter != p.MaxIterations || knobs.maxTok != p.MaxTokens || knobs.runTimeout != p.RunTimeout {
				t.Fatalf("resolveKnobs repaired a resolved spec: %+v vs policy %d/%d/%s", knobs, p.MaxIterations, p.MaxTokens, p.RunTimeout)
			}
			if knobs.spiralWindow != p.TerminationPolicy.NavSpiralWindow {
				t.Fatalf("resolveKnobs spiral window %d != resolved %d", knobs.spiralWindow, p.TerminationPolicy.NavSpiralWindow)
			}

			// resolveTerminationPolicy: the default-filling half must be inert.
			if got := resolveTerminationPolicy(p.TerminationPolicy, 0); got != p.TerminationPolicy {
				t.Fatalf("resolveTerminationPolicy repaired a resolved policy: %+v vs %+v", got, p.TerminationPolicy)
			}

			// verifyTimeout: the lazy max(runTimeout, 5m) branch must never be
			// consulted — Resolve already materialized it.
			if got := verifyTimeout(cfg, p.RunTimeout); got != p.VerifyTimeout {
				t.Fatalf("verifyTimeout(%s) = %s, resolved %s", p.RunTimeout, got, p.VerifyTimeout)
			}

			// The triple ReviewRounds default (review.go / review_pass.go /
			// configrecord.go) keys on ReviewRounds <= 0.
			if p.ReviewRounds <= 0 {
				t.Fatalf("resolved ReviewRounds = %d — the legacy `<= 0 → default` branches would fire", p.ReviewRounds)
			}
			rv := &reviewState{maxRounds: p.ReviewRounds}
			if rv.maxRounds <= 0 {
				t.Fatal("review round fallback reachable")
			}
		})
	}
}

// TestResolveRejectsRatherThanRepairs pins the S6b semantics: an out-of-range
// requested value is a startup ERROR, never a lazy repair (an explicit zero
// stays valid only where its schema says "disabled").
func TestResolveRejectsRatherThanRepairs(t *testing.T) {
	for name, req := range map[string]runspec.RequestedConfig{
		"zero max iterations": {MaxIterations: rsInt(0)},
		"negative tokens":     {MaxTokens: rsInt(-1)},
		"zero run timeout":    {RunTimeout: rsDuration(0)},
		"zero review rounds":  {ReviewRounds: rsInt(0)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runspec.Resolve(req); err == nil {
				t.Fatal("Resolve accepted an out-of-range value the legacy path would have silently repaired")
			}
		})
	}
}

// TestLoopsAssertCompletenessAtEntry: a zero ResolvedSpec (the only
// non-Resolve value constructible) is refused by BOTH loops before any
// model call, sandbox touch, or record write.
func TestLoopsAssertCompletenessAtEntry(t *testing.T) {
	for name, loop := range map[string]LoopFunc{"Run": Run, "RunNative": RunNative} {
		t.Run(name, func(t *testing.T) {
			res, err := loop(context.Background(), runspec.ResolvedSpec{}, Runtime{}, Content{Task: "x"})
			if err == nil {
				t.Fatal("loop accepted a zero ResolvedSpec")
			}
			if res != nil {
				t.Fatalf("refusal returned a result: %+v", res)
			}
			var serr *SetupError
			if !errors.As(err, &serr) || serr.Kind != "invalid_config" {
				t.Fatalf("want invalid_config SetupError, got %v", err)
			}
		})
	}
}

// TestRecordProjectionMatchesLegacyLazyPath: for every variant, the S6a
// record projection built from the HELD resolved value is byte-identical to
// the legacy lazy re-derivation the goldens were built on — the "record path
// re-deriving from the same complete value stays byte-identical" clause.
func TestRecordProjectionMatchesLegacyLazyPath(t *testing.T) {
	for name, req := range unreachabilityVariants() {
		t.Run(name, func(t *testing.T) {
			spec, _, err := runspec.Resolve(req)
			if err != nil {
				t.Fatal(err)
			}
			p := spec.Policy()
			legacyCfg := Config{
				DisableMemoryStore: p.DisableMemoryStore, Persona: p.Persona, MemoryScope: p.MemoryScope,
				BootContext: p.BootContext, StandingContext: p.StandingContext, Stream: p.Stream,
				MinIsolation: p.MinIsolation, RequireNetworkOff: p.RequireNetworkOff,
				MaxIterations: p.MaxIterations, MaxTokens: p.MaxTokens, RunTimeout: p.RunTimeout, VerifyTimeout: p.VerifyTimeout,
				ReasoningEffort: p.ReasoningEffort, PromptProfile: p.PromptProfile, CodeAct: p.CodeAct,
				ReproFirst: p.ReproFirst, ReproGate: p.ReproGate, BatchReads: p.BatchReads,
				ReadWindow: p.ReadWindow, ReadOutline: p.ReadOutline, MaxWallClock: p.MaxWallClock,
				MaxTotalTokens: p.MaxTotalTokens, MaxTotalCostUSD: p.MaxTotalCostUSD, AllowUnpricedSpend: p.AllowUnpricedSpend,
				SolverModel: p.SolverModel, VerifyCmd: p.VerifyCmd, AutoVerify: p.AutoVerify, AutoVerifySoft: p.AutoVerifySoft,
				SkipVerifyBaseline: p.SkipVerifyBaseline, AbortOnRedBaseline: p.AbortOnRedBaseline, VerifyLastRun: p.VerifyLastRun,
				ChurnNudgeRuns: p.ChurnNudgeRuns, VerifyContinue: p.VerifyContinue,
				TestFence: p.TestFence, DiffScope: p.DiffScope, ReviewPolicy: ReviewPolicy(p.ReviewPolicy),
				RequireDiff: p.RequireDiff, ReviewUnverified: p.ReviewUnverified, ReviewRounds: p.ReviewRounds,
				FinishNudgeWindow: p.FinishNudgeWindow, DiagnoseCmd: p.DiagnoseCmd, DiagnoseAfterEdits: p.DiagnoseAfterEdits,
				TerminationPolicy: p.TerminationPolicy, AnswerNudgeWindow: p.AnswerNudgeWindow,
				FinishTool: p.FinishTool, FinishToolTrustsCaller: p.FinishToolTrustsCaller,
			}
			legacy := effectiveConfig(legacyCfg, "tools")
			got := EffectiveConfigFromSpec(spec, Runtime{})
			lb, _ := json.Marshal(legacy)
			gb, _ := json.Marshal(got)
			if string(lb) != string(gb) {
				t.Fatalf("record projections diverge:\nlegacy: %s\nspec:   %s", lb, gb)
			}
		})
	}
}
