package agent

// S6c structural conformance oracles (PROFILES.md §7.4 #1 and #2). The
// behavioral oracle (#3) lives in consumption_oracle_test.go; these two are
// the reflection-anchored halves:
//
//	#1 consumed-and-recorded-exactly-once: every runspec.PolicyValue field is
//	   classified (loop-consumed / gate-consumed / record-only /
//	   builder-consumed / excluded-with-reason) with a NAMED consumption site,
//	   and appears in the canonical policy serialization exactly once. A new
//	   field fails until classified.
//	#2 profile-threading completeness: every profile.FieldID declared by any
//	   registered exec profile provably LANDS in the resolved PolicyValue —
//	   resolving a profile yields the profile's declared value at the mapped
//	   field (the resolver-side proof the behavioral oracle builds on).

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/runspec"
)

type policyFieldClass struct {
	class string // loop-consumed | gate-consumed | record-only | builder-consumed
	site  string // the consuming component (or why record-only/builder)
}

const (
	loopConsumed    = "loop-consumed"
	gateConsumed    = "gate-consumed"
	recordOnly      = "record-only"
	builderConsumed = "builder-consumed"
)

// policyFieldClasses is oracle #1's classification: PolicyValue field → class
// + named consumption site. Behavioral coverage: profile-covered fields are
// asserted in consumption_oracle_test.go; the named non-profile sites are
// covered by the referenced behavior suites (which drive the loops through
// the same resolved specs).
var policyFieldClasses = map[string]policyFieldClass{
	"EffectiveProtocol":      {recordOnly, "loop-selected wire protocol, recorded beside the policy (ConfigRecord.EffectiveProtocol)"},
	"DisableMemoryStore":     {gateConsumed, "gates.finish memory-store suppression (memory_async_test)"},
	"Memory":                 {builderConsumed, "binaries construct mneme when true; the loop consumes Runtime.Memory"},
	"Persona":                {loopConsumed, "withPersona system-prompt prefix (loop seeding)"},
	"MemoryScope":            {loopConsumed, "scopeOrDefault recall/store namespace (memory tests)"},
	"BootContext":            {loopConsumed, "observeEnvironment dependency digest (consumption oracle)"},
	"StandingContext":        {loopConsumed, "standingState per-turn block, native loop (consumption oracle)"},
	"Stream":                 {loopConsumed, "generateOnce streaming path (stream_test)"},
	"MinIsolation":           {loopConsumed, "checkSandboxFloor refusal (isolation_test)"},
	"RequireNetworkOff":      {loopConsumed, "checkSandboxFloor network refusal (isolation/network tests)"},
	"MaxIterations":          {loopConsumed, "loop iteration cap (consumption oracle)"},
	"MaxTokens":              {loopConsumed, "llm.Request.MaxTokens (consumption oracle)"},
	"RunTimeout":             {loopConsumed, "DefaultTools run bound + gate runTimeout (consumption oracle)"},
	"VerifyTimeout":          {gateConsumed, "verifyState.Timeout → verifyRun bound (verifyctx/verify tests)"},
	"ReasoningEffort":        {loopConsumed, "llm.Request.ReasoningEffort (consumption oracle)"},
	"PromptProfile":          {loopConsumed, "resolveSystemPrompt base-prompt selection (prompt_profile_test)"},
	"CodeAct":                {loopConsumed, "resolveSystemPrompt code-act addendum (prompt tests)"},
	"ReproFirst":             {gateConsumed, "repro gate arming + fence new-file carve-out (repro_test)"},
	"ReproGate":              {loopConsumed, "resolveSystemPrompt repro-gate addendum (prompt tests)"},
	"BatchReads":             {loopConsumed, "resolveSystemPrompt batch-reads addendum (consumption oracle)"},
	"ReadWindow":             {loopConsumed, "DefaultTools ReadOptions.Window (consumption oracle)"},
	"ReadOutline":            {loopConsumed, "DefaultTools ReadOptions.Outline (consumption oracle)"},
	"MaxWallClock":           {loopConsumed, "wall-clock budget checks + loopCtx deadline (budget tests)"},
	"MaxTotalTokens":         {loopConsumed, "token budget turn-boundary check (budget_test)"},
	"MaxTotalCostUSD":        {loopConsumed, "dollarBudgetStop (budget tests)"},
	"AllowUnpricedSpend":     {loopConsumed, "dollarBudgetStop fail-open opt-in (budget_metered_repro_test)"},
	"SolverModel":            {loopConsumed, "Spend.Add solver pricing key (spend tests)"},
	"VerifyCmd":              {gateConsumed, "verifyState.Cmd → closing verification gate (verify tests)"},
	"AutoVerify":             {gateConsumed, "resolveAutoVerify derivation arming (consumption oracle)"},
	"AutoVerifySoft":         {gateConsumed, "verifyCompletionFailure soft-accept (consumption oracle)"},
	"SkipVerifyBaseline":     {gateConsumed, "measureVerifyBaseline opt-out (verify_baseline_test)"},
	"AbortOnRedBaseline":     {gateConsumed, "redBaselineRefusal (verify_baseline_test)"},
	"VerifyLastRun":          {gateConsumed, "verifyTermination heuristic fallback (loop tests)"},
	"ChurnNudgeRuns":         {loopConsumed, "turnTracker.churnNudge threshold (consumption oracle)"},
	"VerifyContinue":         {gateConsumed, "gates.finish continue-on-fail (consumption oracle)"},
	"TestFence":              {gateConsumed, "applyTestFence tool wrap + closing re-hash (fence tests)"},
	"DiffScope":              {gateConsumed, "applyDiffScope tool wrap + scopeCheck (diff_scope_test)"},
	"ReviewPolicy":           {gateConsumed, "effectiveReviewRequired + newReviewState (review gate tests)"},
	"RequireDiff":            {gateConsumed, "requireDiffFailure empty-diff gate (consumption oracle)"},
	"ReviewUnverified":       {gateConsumed, "reviewUnverified salvage pass (review_absence_repro_test)"},
	"ReviewRounds":           {gateConsumed, "reviewState.maxRounds repair budget (review gate tests)"},
	"FinishNudgeWindow":      {loopConsumed, "turnTracker.finishNudge near-cap finisher (consumption oracle)"},
	"DiagnoseCmd":            {loopConsumed, "turnTracker.diagnostics source (diagnose_test)"},
	"DiagnoseAfterEdits":     {loopConsumed, "turnTracker.diagnostics stuck threshold (diagnose_test)"},
	"TerminationPolicy":      {loopConsumed, "spiralState/tracker detector thresholds (consumption oracle: nav_spiral_window)"},
	"AnswerNudgeWindow":      {loopConsumed, "near-cap answer-forcer, native loop (consumption oracle)"},
	"FinishTool":             {loopConsumed, "first-class terminal tool, native loop (finish-tool tests)"},
	"FinishToolTrustsCaller": {gateConsumed, "gates.finish trusted-finish path (require_diff_trusted_test)"},
	"Worktree":               {builderConsumed, "binaries create/require the worktree before the loop starts"},
}

// TestPolicyValueFieldsClassifiedAndSerializedOnce is oracle #1.
func TestPolicyValueFieldsClassifiedAndSerializedOnce(t *testing.T) {
	typ := reflect.TypeOf(runspec.PolicyValue{})
	seenTags := map[string]string{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		entry, ok := policyFieldClasses[f.Name]
		if !ok {
			t.Errorf("PolicyValue field %s has no classification — a new policy field must name its class and consumption site", f.Name)
			continue
		}
		switch entry.class {
		case loopConsumed, gateConsumed, recordOnly, builderConsumed:
		default:
			t.Errorf("PolicyValue field %s has invalid class %q", f.Name, entry.class)
		}
		if entry.site == "" {
			t.Errorf("PolicyValue field %s names no consumption site", f.Name)
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Errorf("PolicyValue field %s has no JSON tag — it would silently drop out of the canonical serialization", f.Name)
			continue
		}
		if prev, dup := seenTags[tag]; dup {
			t.Errorf("JSON tag %q appears on both %s and %s — a field must serialize exactly once", tag, prev, f.Name)
		}
		seenTags[tag] = f.Name
	}
	for name := range policyFieldClasses {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("stale policy classification for %s", name)
		}
	}

	// Every classified field appears in the CANONICAL serialization exactly
	// once: serialize a fully-populated value (omitempty must not hide any
	// field) and compare key sets.
	spec, _, err := runspec.Resolve(fullyPopulatedRequest())
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(spec.CanonicalPolicy(), &keys); err != nil {
		t.Fatalf("canonical policy is not a JSON object: %v", err)
	}
	for tag, field := range seenTags {
		if _, ok := keys[tag]; !ok {
			t.Errorf("field %s (tag %q) missing from the canonical serialization of a fully-populated policy", field, tag)
		}
	}
	for tag := range keys {
		if _, ok := seenTags[tag]; !ok {
			t.Errorf("canonical serialization carries unclassified key %q", tag)
		}
	}
}

func fullyPopulatedRequest() runspec.RequestedConfig {
	i, b, s2, d, f := 7, true, "x", 7*time.Second, 1.5
	fence := []string{"*_test.go"}
	scope := []string{"pkg/**"}
	proto := "tools"
	tp := runspec.TerminationPolicy{Version: "t", MaxRepeats: 1, MaxReasoningRepeats: 2, MaxStagnant: 3, NavSpiralWindow: 4, WanderMultiple: 5, FrontierCap: 6, GreenRepeatThreshold: 7}
	wt := "required"
	return runspec.RequestedConfig{
		EffectiveProtocol: &proto, DisableMemoryStore: &b, Memory: &b, Persona: &s2,
		BootContext: &b, StandingContext: &b, Stream: &b,
		RequireNetworkOff: &b, MaxIterations: &i, MaxTokens: &i,
		RunTimeout: &d, VerifyTimeout: &d, ReasoningEffort: &s2, PromptProfile: &s2,
		CodeAct: &b, ReproFirst: &b, ReproGate: &b, BatchReads: &b,
		ReadWindow: &i, ReadOutline: &b, MaxWallClock: &d, MaxTotalTokens: &i,
		MaxTotalCostUSD: &f, AllowUnpricedSpend: &b, SolverModel: &s2, VerifyCmd: &s2,
		AutoVerify: &b, AutoVerifySoft: &b, SkipVerifyBaseline: &b, AbortOnRedBaseline: &b,
		VerifyLastRun: &b, ChurnNudgeRuns: &i, VerifyContinue: &b,
		TestFence: &fence, DiffScope: &scope, ReviewPolicy: &i, RequireDiff: &b,
		ReviewUnverified: &b, ReviewRounds: &i, FinishNudgeWindow: &i,
		DiagnoseCmd: &s2, DiagnoseAfterEdits: &i, TerminationPolicy: &tp,
		AnswerNudgeWindow: &i, FinishTool: &s2, FinishToolTrustsCaller: &b, Worktree: &wt,
	}
}

// fieldIDToPolicy maps every profile.FieldID to the PolicyValue location the
// resolver must land it in (oracle #2's mapping — itself completeness-checked).
var fieldIDToPolicy = map[profile.FieldID]func(runspec.PolicyValue) any{
	profile.MaxItersField:          func(p runspec.PolicyValue) any { return p.MaxIterations },
	profile.MaxTokensField:         func(p runspec.PolicyValue) any { return p.MaxTokens },
	profile.FinishNudgeField:       func(p runspec.PolicyValue) any { return p.FinishNudgeWindow },
	profile.RunTimeoutField:        func(p runspec.PolicyValue) any { return p.RunTimeout },
	profile.EffortField:            func(p runspec.PolicyValue) any { return p.ReasoningEffort },
	profile.WorktreeField:          func(p runspec.PolicyValue) any { return p.Worktree },
	profile.VerifyContinueField:    func(p runspec.PolicyValue) any { return p.VerifyContinue },
	profile.AutoVerifyField:        func(p runspec.PolicyValue) any { return p.AutoVerify },
	profile.AutoVerifySoftField:    func(p runspec.PolicyValue) any { return p.AutoVerifySoft },
	profile.MemoryField:            func(p runspec.PolicyValue) any { return p.Memory },
	profile.BootContextField:       func(p runspec.PolicyValue) any { return p.BootContext },
	profile.StandingContextField:   func(p runspec.PolicyValue) any { return p.StandingContext },
	profile.BatchReadsField:        func(p runspec.PolicyValue) any { return p.BatchReads },
	profile.RequireDiffField:       func(p runspec.PolicyValue) any { return p.RequireDiff },
	profile.ReadWindowField:        func(p runspec.PolicyValue) any { return p.ReadWindow },
	profile.ReadOutlineField:       func(p runspec.PolicyValue) any { return p.ReadOutline },
	profile.ChurnNudgeRunsField:    func(p runspec.PolicyValue) any { return p.ChurnNudgeRuns },
	profile.NavSpiralWindowField:   func(p runspec.PolicyValue) any { return p.TerminationPolicy.NavSpiralWindow },
	profile.AnswerNudgeWindowField: func(p runspec.PolicyValue) any { return p.AnswerNudgeWindow },
}

// TestProfileFieldsLandInResolvedSpec is oracle #2: resolving each registered
// exec profile yields the profile's own declared value at the mapped
// PolicyValue location, for every declared FieldID.
func TestProfileFieldsLandInResolvedSpec(t *testing.T) {
	for _, f := range profile.ProfileFields {
		if _, ok := fieldIDToPolicy[f]; !ok {
			t.Errorf("profile field %s has no PolicyValue mapping — extend fieldIDToPolicy", f)
		}
	}
	for _, name := range []string{"coding-v1", "coding-v2", "interactive-v1", "interactive-v2", "observe-v1", "eval-swe-v1"} {
		name := name
		t.Run(name, func(t *testing.T) {
			exec, err := profile.ExecByName(name)
			if err != nil {
				t.Fatal(err)
			}
			trust := "trusted-local"
			spec, _, err := runspec.Resolve(runspec.RequestedConfig{TrustProfile: &trust, ExecProfileName: &name})
			if err != nil {
				t.Fatal(err)
			}
			pol := spec.Policy()
			n, _, err := profile.Resolve(profile.TrustPosture{Selected: profile.TrustedLocal, Posture: profile.FloorFor(profile.TrustedLocal)}, exec, profile.Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			// Zero-declared budget knobs mean "caller decides"; the resolver
			// backfills them with the base defaults (the historical lazy
			// semantics, equivalence-pinned) — the landing check expects that.
			wantIters, wantTokens, wantTimeout := n.MaxIters, n.MaxTokens, n.RunTimeout
			if wantIters <= 0 {
				wantIters = runspec.DefaultMaxIterations
			}
			if wantTokens <= 0 {
				wantTokens = runspec.DefaultMaxTokens
			}
			if wantTimeout <= 0 {
				wantTimeout = runspec.DefaultRunTimeout
			}
			want := map[profile.FieldID]any{
				profile.MaxItersField: wantIters, profile.MaxTokensField: wantTokens,
				profile.FinishNudgeField: n.FinishNudge, profile.RunTimeoutField: wantTimeout,
				profile.EffortField: n.Effort, profile.WorktreeField: n.Worktree,
				profile.VerifyContinueField: n.VerifyContinue, profile.AutoVerifyField: n.AutoVerify,
				profile.AutoVerifySoftField: n.AutoVerifySoft, profile.MemoryField: n.Memory,
				profile.BootContextField: n.BootContext, profile.StandingContextField: n.StandingContext,
				profile.BatchReadsField: n.BatchReads, profile.RequireDiffField: n.RequireDiff,
				profile.ReadWindowField: n.ReadWindow, profile.ReadOutlineField: n.ReadOutline,
				profile.ChurnNudgeRunsField:    n.ChurnNudgeRuns,
				profile.AnswerNudgeWindowField: n.AnswerNudgeWindow,
			}
			for f, w := range want {
				if got := fieldIDToPolicy[f](pol); got != w {
					t.Errorf("%s: profile %s declared %v, resolved spec carries %v", f, name, w, got)
				}
			}
			// NavSpiralWindow: the profile value lands in the termination policy,
			// except the zero-means-default cases the resolver backfills.
			wantNav := n.NavSpiralWindow
			if wantNav <= 0 {
				wantNav = runspec.DefaultTerminationPolicy().NavSpiralWindow
			}
			if got := pol.TerminationPolicy.NavSpiralWindow; got != wantNav {
				t.Errorf("nav_spiral_window: profile %s → %v, resolved %v", name, wantNav, got)
			}
		})
	}
}
