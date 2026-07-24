package headless

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
)

// setupReportPath is the -report path armed by run() after flag parsing.
// failSetup writes a small markdown report to this path on exit-1.
var setupReportPath string

func configureSetupReport(path string) { setupReportPath = path }

// The CLI output contract (docs/specs/CLI-SCRIPTABLE.md, Tier 1). stdout is the DATA channel
// (D1): a single final payload in text/json mode. stderr carries the live trace
// and banners. The result object (D3) is emitted for EVERY RunResult — including
// error outcomes — and exit codes (D4) carry the outcome class so a script can
// branch on $?.

// outputFormat is the resolved -format.
type outputFormat string

const (
	formatText   outputFormat = "text"
	formatJSON   outputFormat = "json"
	formatNDJSON outputFormat = "ndjson"
)

// schemaVersion stamps every machine-readable object so a downstream parser can
// detect a contract change instead of silently misreading new fields.
// v3: the result object gains `solver_cost_usd`, `reviewer_cost_usd`,
// `planner_cost_usd`, and `total_cost_usd` (COST-REPORTING).
// v5: the result object gains `best_of` and `selector_cost_usd` for Best-of-N.
// v6: `review.status` records review-gate failure/semantic status.
// v7: the result object gains the armed role model ids (`review_model`,
// `plan_model`, `select_model`) so consumers stop re-parsing argv for them.
// v8: the result object gains `rescued_from` — the pre-upgrade outcome when
// the closing gates rescued a cap/kill exit to answered ("" = never rescued),
// so a cap-rescued exit-0 is distinguishable from a model-claimed answer.
// v9: the result object gains typed `guarantees` evidence.
// v10: `guarantees.diff.workspace_effect` and no-op-answer degradation added.
// v11: proof bundle path, manifest hash, and bundle warning added.
// v12: outer proof-bundle completeness status added.
const schemaVersion = 12

// validateFormat resolves the -format flag, rejecting unknown values up front.
func validateFormat(s string) (outputFormat, error) {
	switch outputFormat(s) {
	case formatText, formatJSON, formatNDJSON:
		return outputFormat(s), nil
	default:
		return "", fmt.Errorf("unknown -format %q (want 'text', 'json', or 'ndjson')", s)
	}
}

// validateProtocol rejects unknown -protocol values up front (review #11): the
// old check treated ANYTHING non-"tools" as text, so a typo like -protocol=tool
// silently disabled native tool-calling — the production path — and the run
// quietly degraded instead of failing loudly.
func validateProtocol(s string) error {
	switch s {
	case "tools", "text":
		return nil
	default:
		return fmt.Errorf("unknown -protocol %q (want 'tools' or 'text')", s)
	}
}

// jsonUsage mirrors llm.Usage with the snake_case wire names the result object
// promises (D3). A dedicated struct keeps the JSON contract independent of the
// internal field names.
type jsonUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

func usageOf(u llm.Usage) jsonUsage {
	return jsonUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.CachedTokens,
		ReasoningTokens:  u.ReasoningTokens,
	}
}

// jsonError is the {kind, message} shape carried by a result's `error` field
// (provider_error) and by a cli_error object's `error` field (D3).
type jsonError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// resultObject is the D3 result schema. Every top-level key is ALWAYS present;
// the nullable ones are pointers so an unset value serializes to JSON null rather
// than a zero value (D3/O8):
//   - Answer: a string for {answered, unverified}, else null.
//   - Reason: a string for every non-answered outcome, else "".
//   - Error: an object for provider_error, else null.
//   - TranscriptPath: a string, or null when the transcript write failed (O7).
//   - WorktreePath: the kept -worktree checkout path, or null.
//   - PatchPath: the kept -worktree patch path, or null.
//   - PatchError: non-empty when collecting a -worktree patch failed after the run.
type resultObject struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	Outcome       string `json:"outcome"`
	// RescuedFrom is the pre-upgrade outcome (e.g. "hit_cap") when the closing
	// gates rescued this run to answered; "" when the model finished itself.
	RescuedFrom string `json:"rescued_from"`
	ExitCode    int    `json:"exit_code"`
	Task        string `json:"task"`
	Model       string `json:"model"`
	// Armed role model ids (v7) — null when the role is off. These record the
	// CONFIGURATION; the per-role reports (review/plan/best_of) record what ran.
	ReviewModel *string   `json:"review_model"`
	PlanModel   *string   `json:"plan_model"`
	SelectModel *string   `json:"select_model"`
	Answer      *string   `json:"answer"`
	Reason      string    `json:"reason"`
	Iterations  int       `json:"iterations"`
	Usage       jsonUsage `json:"usage"`
	// Prompt-cache efficiency (Measurement 3).
	CacheSumExpectedCached int     `json:"sum_expected_cached"`
	CacheSumCached         int     `json:"sum_cached"`
	CacheMiss              int     `json:"cache_miss"`
	CacheHitPct            float64 `json:"cache_hit_pct"`
	StartedAt              string  `json:"started_at"`
	EndedAt                string  `json:"ended_at"`
	TranscriptPath         *string `json:"transcript_path"`
	WorktreePath           *string `json:"worktree_path"`
	PatchPath              *string `json:"patch_path"`
	PatchError             string  `json:"patch_error"`
	BundlePath             *string `json:"bundle_path"`
	BundleManifestSHA256   *string `json:"bundle_manifest_sha256"`
	BundleWarning          string  `json:"bundle_warning,omitempty"`
	BundleStatus           string  `json:"bundle_status"`
	closingVerification    *agent.VerificationRecord
	configRecord           *agent.ConfigRecord
	Error                  *jsonError `json:"error"`
	// Review is the review-gate report (rounds, findings + fates, reviewer
	// usage) — null when no -review-model was configured (D3: every key always
	// present, unset serializes to null).
	Review     *agent.ReviewReport `json:"review"`
	Guarantees agent.Guarantees    `json:"guarantees"`
	Repro      *agent.ReproReport  `json:"repro,omitempty"`
	// Plan is the plan-stage report (the plan handed to the solver or the
	// skip reason, plus the planner's own usage) — null when no -plan-model
	// was configured. Same contract as Review: per-role cost stays
	// attributable in the machine output.
	Plan *agent.PlanReport `json:"plan"`
	// BestOf is the Best-of-N selection report — null unless -best-of >= 2.
	BestOf *bestOfReport `json:"best_of"`
	// VerifyBaselineRed is true when -verify-cmd already failed on the untouched
	// workspace before the first model call.
	VerifyBaselineRed bool `json:"verify_baseline_red,omitempty"`
	// VerifyBaselineOut is the failing output of the baseline -verify-cmd run,
	// clipped like other observations. Empty when the baseline was green.
	VerifyBaselineOut string `json:"verify_baseline_out,omitempty"`
	// VerifyInfra is true when -verify-cmd could not give a code verdict because
	// it hit a recognized environment fault.
	VerifyInfra bool `json:"verify_infra,omitempty"`
	// VerifyInfraSignature is the matched environment-fault signature.
	VerifyInfraSignature string `json:"verify_infra_signature,omitempty"`

	// BaselineCommit is the full SHA the -worktree run's detached checkout is
	// based on — HEAD when the main checkout was clean, the dirty-tree snapshot
	// commit when dirty. null when -worktree was off.
	BaselineCommit *string `json:"baseline_commit"`
	// BaselineDirtyFiles lists the paths that differ between HEAD and the
	// snapshot commit (sorted), omitted when clean or when -worktree was off.
	BaselineDirtyFiles []string `json:"baseline_dirty_files,omitempty"`

	// Cost reporting (COST-REPORTING). Each role field is the priced cost when
	// eval.CostOf ok, else null. total_cost_usd is the sum over roles that
	// RAN, but null if any role that ran is unpriced (D3: never conflate
	// unknown with free). cost_source is "metered" if all roles used
	// provider-reported costs, "modeled" if all used the price table, or
	// "mixed".
	SolverCost   *float64 `json:"solver_cost_usd"`
	ReviewerCost *float64 `json:"reviewer_cost_usd"`
	PlannerCost  *float64 `json:"planner_cost_usd"`
	SelectorCost *float64 `json:"selector_cost_usd"`
	TotalCost    *float64 `json:"total_cost_usd"`
	CostSource   *string  `json:"cost_source"`
}

// cliErrorObject is what json mode emits to stdout for an exit-1 setup error that
// produced no RunResult (no provider key, sandbox build failure). It keeps stdout
// valid JSON so a consumer parses unconditionally and branches on exit_code/outcome
// rather than on whether stdout happened to be empty (D3/D4, closes O2).
type cliErrorObject struct {
	SchemaVersion int       `json:"schema_version"`
	Outcome       string    `json:"outcome"` // always "cli_error"
	ExitCode      int       `json:"exit_code"`
	Error         jsonError `json:"error"`

	WorktreePath *string `json:"worktree_path"`
	PatchPath    *string `json:"patch_path"`

	// Cost reporting (COST-REPORTING). Always null for setup errors.
	SolverCost   *float64 `json:"solver_cost_usd"`
	ReviewerCost *float64 `json:"reviewer_cost_usd"`
	PlannerCost  *float64 `json:"planner_cost_usd"`
	SelectorCost *float64 `json:"selector_cost_usd"`
	TotalCost    *float64 `json:"total_cost_usd"`
	CostSource   *string  `json:"cost_source"`
}

// failSetup reports an exit-1 setup error that produced no RunResult (no provider
// key, sandbox build failure). It keeps stdout valid for the chosen format so a
// consumer parses unconditionally (closes O2/O9): json emits the bare cli_error
// object; ndjson emits it as the terminal `result` event (the stream is always
// typed events); text prints the message to stderr. Always returns exit code 1.
func failSetup(stdout, stderr io.Writer, format outputFormat, kind, msg string) int {
	appendSetupErrorLedger()
	cli := cliErrorObject{
		SchemaVersion: schemaVersion,
		Outcome:       "cli_error",
		ExitCode:      1,
		Error:         jsonError{Kind: kind, Message: msg},
	}
	switch format {
	case formatJSON:
		writeJSON(stdout, cli)
	case formatNDJSON:
		writeResultEvent(stdout, cli)
	default: // text
		fmt.Fprintln(stderr, msg)
	}
	if setupReportPath != "" {
		content := "# driver-agent report (exit=1)\ncli_error\n- outcome: cli_error\n- kind: " + kind + "\n- error: " + msg + "\n"
		if err := os.WriteFile(setupReportPath, []byte(content), 0644); err != nil {
			fmt.Fprintln(stderr, "setup report write failed:", err)
		}
	}
	return 1
}

// ---- ndjson event stream (D2) ----
//
// In ndjson mode the data channel IS the event stream: one JSON object per
// Observer event to stdout as the run progresses, terminated by a single `result`
// event. The event types below map 1:1 onto agent.Observer; the terminal event
// nests the D3 payload under `result` so `type` never collides with a result field
// (O6), and a setup error is emitted through the SAME terminal event so the stream
// contract holds on the error path too (O9).
type (
	evtIteration struct {
		Type string `json:"type"`
		I    int    `json:"i"`
		Max  int    `json:"max"`
	}
	evtText struct {
		Type string `json:"type"` // "model", "observation", or "note"
		Text string `json:"text"`
	}
	evtDone struct {
		Type   string `json:"type"`
		Answer string `json:"answer"`
	}
	evtResult struct {
		Type   string `json:"type"` // always "result"
		Result any    `json:"result"`
	}
	// Review-gate events (slice 1e; schema v2). The finding event nests the
	// full typed finding rather than flattening it, so the schema tracks
	// agent.ReviewFinding without a second copy.
	evtReviewStart struct {
		Type  string `json:"type"` // "review_start"
		Round int    `json:"round"`
		Model string `json:"model,omitempty"`
	}
	evtReviewFinding struct {
		Type    string              `json:"type"` // "review_finding"
		Finding agent.ReviewFinding `json:"finding"`
	}
	evtReviewVerdict struct {
		Type     string `json:"type"` // "review_verdict"
		Round    int    `json:"round"`
		Blocking int    `json:"blocking"`
		Summary  string `json:"summary,omitempty"`
	}
)

// ndjsonObserver renders the live loop trace as ndjson events to w (stdout in
// ndjson mode). Unlike the human writerObserver it does NOT oneLine/clip: the
// Observer hands the full (already loop-bounded) text, and a machine consumer
// wants it whole. It satisfies agent.Observer.
type ndjsonObserver struct{ w io.Writer }

func (o ndjsonObserver) Iteration(i, max int) { writeJSON(o.w, evtIteration{"iteration", i, max}) }
func (o ndjsonObserver) Model(reply string)   { writeJSON(o.w, evtText{"model", reply}) }
func (o ndjsonObserver) Observation(t string) { writeJSON(o.w, evtText{"observation", t}) }
func (o ndjsonObserver) Note(msg string)      { writeJSON(o.w, evtText{"note", msg}) }
func (o ndjsonObserver) Done(answer string)   { writeJSON(o.w, evtDone{"done", answer}) }

// ndjsonObserver also implements agent.ReviewObserver (discovered by
// type-assertion, like DeltaObserver), so review-gate progress flows on the
// event stream as typed events instead of opaque notes.
func (o ndjsonObserver) ReviewStart(round int, model string) {
	writeJSON(o.w, evtReviewStart{"review_start", round, model})
}
func (o ndjsonObserver) ReviewFinding(f agent.ReviewFinding) {
	writeJSON(o.w, evtReviewFinding{"review_finding", f})
}
func (o ndjsonObserver) ReviewVerdict(blocking, round int, summary string) {
	writeJSON(o.w, evtReviewVerdict{"review_verdict", round, blocking, summary})
}

// writeResultEvent emits the terminal `result` event wrapping any D3 payload (a
// resultObject or a cliErrorObject).
func writeResultEvent(w io.Writer, payload any) { writeJSON(w, evtResult{"result", payload}) }

// exitCodeFor maps a terminal Outcome to its exit-code CLASS (D4). The classes are
// what let a script react differently: 3 = retry with a bigger budget, 4 = the
// agent is stuck (swap model, don't just retry), 5 = infra (backoff+retry), 6 =
// policy (never retry), 2 = an answer exists but failed the gate (inspect). An
// unrecognized outcome falls to 1 (a setup/internal error) rather than masquerading
// as success.
func exitCodeFor(o agent.Outcome) int {
	switch o {
	case agent.Answered:
		return 0
	case agent.Unverified:
		return 2
	case agent.HitCap, agent.HitDeadline, agent.HitContextLimit, agent.HitBudget:
		return 3
	case agent.KilledRepeat, agent.KilledSpiral, agent.KilledStagnant:
		return 4
	case agent.ProviderErr:
		return 5
	case agent.RefusedUnsafe:
		return 6
	case agent.Canceled:
		return 7
	case agent.ScopeViolation:
		return 8
	default:
		return 1
	}
}

// buildResult assembles the D3 object from a finished run. transcriptPath is the
// path the transcript landed at, or "" if the write failed/was disabled (→ null).
// worktreePath and patchPath are optional kept -worktree artifacts (→ null when empty).
// A third optional artifact string is a patch collection error; when set it is
// emitted as patch_error and forces the result exit code to setup-error class 1.
func buildResultWithPrice(res *agent.RunResult, model, transcriptPath, baselineCommit string, baselineDirtyFiles []string, price PriceLookup, artifactPaths ...string) resultObject {
	return buildResultWithBestOfPrice(res, model, transcriptPath, nil, baselineCommit, baselineDirtyFiles, price, artifactPaths...)
}

func buildResultWithBestOf(res *agent.RunResult, model, transcriptPath string, bestOf *bestOfReport, baselineCommit string, baselineDirtyFiles []string, artifactPaths ...string) resultObject {
	return buildResultWithBestOfPrice(res, model, transcriptPath, bestOf, baselineCommit, baselineDirtyFiles, nil, artifactPaths...)
}

func buildResultWithBestOfPrice(res *agent.RunResult, model, transcriptPath string, bestOf *bestOfReport, baselineCommit string, baselineDirtyFiles []string, price PriceLookup, artifactPaths ...string) resultObject {
	exit := exitCodeFor(res.Outcome)
	r := resultObject{
		SchemaVersion:          schemaVersion,
		ID:                     res.ID,
		Outcome:                string(res.Outcome),
		RescuedFrom:            string(res.RescuedFrom),
		ExitCode:               exit,
		Task:                   res.Task,
		Model:                  model,
		Reason:                 res.Reason,
		Iterations:             res.Iterations,
		Usage:                  usageOf(res.Usage),
		CacheSumExpectedCached: res.CacheSumExpectedCached,
		CacheSumCached:         res.CacheSumCached,
		CacheMiss:              res.CacheMiss,
		CacheHitPct:            res.CacheHitPct,
		StartedAt:              res.StartedAt.UTC().Format(time.RFC3339),
		EndedAt:                res.EndedAt.UTC().Format(time.RFC3339),
		VerifyBaselineRed:      res.VerifyBaselineRed,
		VerifyBaselineOut:      res.VerifyBaselineOut,
		VerifyInfra:            res.VerifyInfra,
		VerifyInfraSignature:   res.VerifyInfraSignature,
		Repro:                  res.Repro,
		Guarantees:             res.Guarantees,
		configRecord:           res.ConfigRecord,
		BundleStatus:           "absent",
		closingVerification:    res.ClosingVerification,
	}
	// Answer is exposed for a verified answer AND for an unverified one — the
	// unverified answer is the defining payload of that outcome (D3, closes O5).
	if res.Outcome == agent.Answered || res.Outcome == agent.Unverified {
		a := res.Answer
		r.Answer = &a
	}
	// Reason is empty on the happy path (the answer carries the payload there).
	if res.Outcome == agent.Answered {
		r.Reason = ""
	}
	if res.Outcome == agent.ProviderErr {
		msg := res.Reason
		if res.Err != nil {
			msg = res.Err.Error()
		}
		r.Error = &jsonError{Kind: "provider_error", Message: msg}
	}
	if transcriptPath != "" {
		r.TranscriptPath = &transcriptPath
	}
	if len(artifactPaths) > 0 && artifactPaths[0] != "" {
		r.WorktreePath = &artifactPaths[0]
	}
	if len(artifactPaths) > 1 && artifactPaths[1] != "" {
		r.PatchPath = &artifactPaths[1]
		r.Guarantees.Diff.PatchRef = artifactPaths[1]
	}
	if len(artifactPaths) > 2 && artifactPaths[2] != "" {
		r.PatchError = artifactPaths[2]
		r.ExitCode = 1
	}
	if baselineCommit != "" {
		bc := baselineCommit
		r.BaselineCommit = &bc
		if len(baselineDirtyFiles) > 0 {
			r.BaselineDirtyFiles = baselineDirtyFiles
		}
	}
	r.Review = res.Review
	r.Plan = res.Plan
	r.BestOf = bestOf

	// Calculate costs (COST-REPORTING).
	var (
		total      float64
		totalKnown = true
		hasMetered bool
		hasModeled bool
	)

	roleRan := func(model string, usage llm.Usage) bool {
		return model != "" || usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0
	}

	// resolveRoleCost prefers metered cost (Usage.Cost) over modeled cost (eval.CostOf).
	resolveRoleCost := func(model string, usage llm.Usage) (*float64, bool) {
		if usage.Cost > 0 {
			hasMetered = true
			return &usage.Cost, true
		}
		if price != nil {
			if cost, ok := price(model, usage); ok {
				hasModeled = true
				return &cost, true
			}
		}
		return nil, false
	}

	// Solver cost.
	if cost, ok := resolveRoleCost(model, res.Usage); ok {
		r.SolverCost = cost
		total += *cost
	} else {
		totalKnown = false
	}

	// Reviewer cost.
	if res.Review != nil && roleRan(res.Review.ReviewerModel, res.Review.Usage) {
		if cost, ok := resolveRoleCost(res.Review.ReviewerModel, res.Review.Usage); ok {
			r.ReviewerCost = cost
			total += *cost
		} else {
			totalKnown = false
		}
	}

	// Planner cost.
	if res.Plan != nil && roleRan(res.Plan.Model, res.Plan.Usage) {
		if cost, ok := resolveRoleCost(res.Plan.Model, res.Plan.Usage); ok {
			r.PlannerCost = cost
			total += *cost
		} else {
			totalKnown = false
		}
	}

	// Selector cost.
	if bestOf != nil && roleRan(bestOf.SelectorModel, bestOf.Usage) {
		// bestOf.Cost is already metered/authoritative if present.
		if bestOf.Usage.Cost > 0 {
			r.SelectorCost = &bestOf.Usage.Cost
			total += bestOf.Usage.Cost
			hasMetered = true
		} else if bestOf.Cost != nil {
			r.SelectorCost = bestOf.Cost
			total += *bestOf.Cost
			hasModeled = true
		} else if price != nil {
			if cost, ok := price(bestOf.SelectorModel, bestOf.Usage); ok {
				r.SelectorCost = &cost
				total += cost
				hasModeled = true
			} else {
				totalKnown = false
			}
		} else {
			totalKnown = false
		}
	}

	if totalKnown {
		r.TotalCost = &total
		var source string
		switch {
		case hasMetered && hasModeled:
			source = "mixed"
		case hasMetered:
			source = "metered"
		case hasModeled:
			source = "modeled"
		}
		if source != "" {
			r.CostSource = &source
		}
	}

	return r
}

// emitResult writes the finished run to stdout in the chosen format (D1: stdout is
// the data channel). text mode prints the answer (when present) followed by the
// machine-readable SUMMARY line; json mode prints the bare result object; ndjson
// mode prints the terminal `result` event that closes the event stream. It returns
// the exit code the process should use.
func emitResult(out io.Writer, format outputFormat, r resultObject) int {
	switch format {
	case formatJSON:
		writeJSON(out, r)
	case formatNDJSON:
		writeResultEvent(out, r) // terminal event closing the event stream.
	default: // text
		if r.Answer != nil {
			fmt.Fprintln(out, *r.Answer)
		}
		fmt.Fprintf(out,
			"SUMMARY outcome=%s id=%s iters=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d cached_tokens=%d reasoning_tokens=%d cache_hit_pct=%.1f sum_expected_cached=%d sum_cached=%d cache_miss=%d\n",
			r.Outcome, r.ID, r.Iterations,
			r.Usage.PromptTokens, r.Usage.CompletionTokens, r.Usage.TotalTokens,
			r.Usage.CachedTokens, r.Usage.ReasoningTokens,
			r.CacheHitPct, r.CacheSumExpectedCached, r.CacheSumCached, r.CacheMiss)
	}
	return r.ExitCode
}

// writeJSON encodes v as a single line of JSON to w. SetEscapeHTML(false) keeps
// characters like < > & literal so an answer containing them round-trips cleanly.
func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // a stdout write error is not separately recoverable.
}
