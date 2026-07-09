package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/AccursedGalaxy/driver-os/agent"
)

// Run sweeps every (Case × Model) pair n times, sequentially, and aggregates the
// trials into a Report. n trials per cell is the point (HP-11): a single run
// measures the dice — the bake-offs flipped the same model pass↔fail across runs
// — so the unit of measurement is a distribution, not one boolean. For a real
// multi-model sweep use RunConcurrent; this is the simple, deterministic path.
func Run(ctx context.Context, cases []Case, models []Model, n int) *Report {
	return RunConcurrent(ctx, cases, models, n, 1, nil)
}

// RunConcurrent is the same sweep with up to `workers` trials in flight at once.
// Trials are independent — each materializes its OWN pristine temp fixture and
// sandbox (trial.go) — so concurrency is safe and the wall-clock of a 30-run
// roster sweep drops from sum-of-runs to ceil(runs/workers) waves. Results are
// written into preallocated per-cell slots (distinct indices, no append race),
// so the report's cell/trial order is identical to the sequential Run regardless
// of completion order. onDone, if non-nil, is called once per finished trial
// (from a worker goroutine — it must be concurrency-safe) for live progress.
func RunConcurrent(ctx context.Context, cases []Case, models []Model, n, workers int, onDone func(Trial)) *Report {
	if n < 1 {
		n = 1
	}
	if workers < 1 {
		workers = 1
	}

	rep := &Report{}
	cellOf := map[[2]int]int{} // (caseIdx, modelIdx) -> index into rep.Cells.
	for ci, c := range cases {
		for mi, m := range models {
			cellOf[[2]int{ci, mi}] = len(rep.Cells)
			rep.Cells = append(rep.Cells, Cell{Case: c.Name, Model: m.Label, Trials: make([]Trial, n)})
		}
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for ci := range cases {
		for mi := range models {
			for idx := 0; idx < n; idx++ {
				wg.Add(1)
				sem <- struct{}{}
				go func(ci, mi, idx int) {
					defer wg.Done()
					defer func() { <-sem }()
					tr := RunTrial(ctx, cases[ci], models[mi], idx+1)
					rep.Cells[cellOf[[2]int{ci, mi}]].Trials[idx] = tr
					if onDone != nil {
						onDone(tr)
					}
				}(ci, mi, idx)
			}
		}
	}
	wg.Wait()
	return rep
}

// Cell is the n trials of one (Case × Model) pair — the unit a metric is
// computed over. The methods are the report's vocabulary: a pass-rate with a
// confidence interval (not a bare fraction), the false-positive rate (the
// honesty number), the outcome histogram, and cost/convergence percentiles.
type Cell struct {
	Case   string
	Model  string
	Trials []Trial

	// PricingCoverage_ and UnpricedModels_ are additive report fields
	// populated by WriteFiles before JSON serialisation so downstream
	// consumers of report.json can see per-cell pricing honesty without
	// recomputing from trials. The methods PricingCoverage() and
	// UnpricedModels() compute the same values on the fly for callers that
	// don't go through WriteFiles (tests, markdown).
	PricingCoverage_ string   `json:"pricing_coverage,omitempty"`
	UnpricedModels_  []string `json:"unpriced_models,omitempty"`
}

// N is the trial count.
func (c Cell) N() int { return len(c.Trials) }

// Passes counts trials the ORACLE passed (the real grade, not the self-report).
func (c Cell) Passes() int {
	n := 0
	for _, t := range c.Trials {
		if t.Pass {
			n++
		}
	}
	return n
}

// Infra counts trials that failed with an infra error (Err != "").
func (c Cell) Infra() int {
	n := 0
	for _, t := range c.Trials {
		if t.Err != "" {
			n++
		}
	}
	return n
}

// PassRate is Passes/N (0 when empty).
func (c Cell) PassRate() float64 {
	if c.N() == 0 {
		return 0
	}
	return float64(c.Passes()) / float64(c.N())
}

// PassRateInfraExcluded is (Pass && Err == "") / (N - Infra). 0 when N == Infra.
func (c Cell) PassRateInfraExcluded() float64 {
	denom := c.N() - c.Infra()
	if denom <= 0 {
		return 0
	}
	passes := 0
	for _, t := range c.Trials {
		if t.Pass && t.Err == "" {
			passes++
		}
	}
	return float64(passes) / float64(denom)
}

// PassRateCI is the Wilson 95% interval for the pass-rate — honest about small
// N. With 7/10 passing it reports ≈[0.40, 0.89], so "0.7" is never mistaken for
// a precise measurement off ten noisy samples.
func (c Cell) PassRateCI() (lo, hi float64) { return wilson(c.Passes(), c.N()) }

// FalsePositives counts trials where the agent claimed done (Answered) but the
// oracle failed it — the dangerous failure the bake-offs surfaced.
func (c Cell) FalsePositives() int {
	n := 0
	for _, t := range c.Trials {
		if t.FalsePositive() {
			n++
		}
	}
	return n
}

// FalsePositiveRate is FalsePositives/N — the metric the closing verification
// gate (VerifyCmd) is meant to drive toward zero.
func (c Cell) FalsePositiveRate() float64 {
	if c.N() == 0 {
		return 0
	}
	return float64(c.FalsePositives()) / float64(c.N())
}

// Outcomes is the histogram of self-reported terminal states across the cell —
// answered / hit_cap / killed_* / unverified — so a failing cell shows HOW it
// failed (looped to cap vs killed by a detector vs a false answer).
func (c Cell) Outcomes() map[agent.Outcome]int {
	h := map[agent.Outcome]int{}
	for _, t := range c.Trials {
		h[t.Outcome]++
	}
	return h
}

// ItersP returns the pth percentile (0..100) of iteration counts. onlyPass
// restricts to passing trials — convergence is only meaningful for runs that
// actually solved the task (a run that looped to the cap tells you nothing about
// how fast success arrives).
func (c Cell) ItersP(p float64, onlyPass bool) int {
	var xs []int
	for _, t := range c.Trials {
		if onlyPass && !t.Pass {
			continue
		}
		xs = append(xs, t.Iters)
	}
	return percentileInt(xs, p)
}

// PromptTokensP is the pth percentile of prompt tokens across all trials — the
// cost axis (capability shows up as token spend, not just pass/fail).
func (c Cell) PromptTokensP(p float64) int {
	xs := make([]int, len(c.Trials))
	for i, t := range c.Trials {
		xs[i] = t.Usage.PromptTokens
	}
	return percentileInt(xs, p)
}

// LatencyP is the pth percentile of per-trial agent wall-clock (ms) across the
// cell — the time axis, told apart from token spend: a model can be cheap per
// token yet slow per run (high reasoning latency, slow tools). 0 when no step
// timing was captured (e.g. a pre-timing trace replay).
func (c Cell) LatencyP(p float64) int64 {
	xs := make([]int64, len(c.Trials))
	for i, t := range c.Trials {
		xs[i] = t.LatencyMs
	}
	return percentileInt64(xs, p)
}

// ReasoningTokensP is the pth percentile of reasoning tokens across all trials —
// where a thinking model's completion spend actually goes (a subset of completion
// tokens, already billed). 0 for non-thinking models, which is the useful signal.
func (c Cell) ReasoningTokensP(p float64) int {
	xs := make([]int, len(c.Trials))
	for i, t := range c.Trials {
		xs[i] = t.Usage.ReasoningTokens
	}
	return percentileInt(xs, p)
}

// CostP is the pth percentile of per-trial USD cost across the cell's PRICED
// trials — the comparable cost-per-run number (a slow, token-hungry passer is
// expensive even at a cheap per-token rate). ok=false when no trial was priced,
// so the report renders "—" rather than a misleading $0.
func (c Cell) CostP(p float64) (cost float64, ok bool) {
	var xs []float64
	for _, t := range c.Trials {
		if t.Priced {
			xs = append(xs, t.Cost)
		}
	}
	if len(xs) == 0 {
		return 0, false
	}
	return percentileFloat(xs, p), true
}

// CostTotal sums the USD cost of every priced trial in the cell — what this
// cell actually cost to measure, reviewer bill included (R4b's sweep-cost line
// underreported 6.5× by pricing only the solver). ok=false when nothing was
// priced. The $/trial percentile column stays solver-only on purpose: it
// compares MODELS, while this compares BILLS.
//
// Ladder trials: t.Cost is already all-in (solver + reviewer + planner summed
// by ladder.priceAttempt across every attempt), so PlanCost/ReviewCost are NOT
// added — they hold the WINNING attempt's components and would double-count.
func (c Cell) CostTotal() (total float64, ok bool) {
	for _, t := range c.Trials {
		if t.Ladder != nil {
			// ladder.Cost already includes solver + reviewer + planner for
			// every attempt; the per-trial ReviewCost/PlanCost are the
			// final-attempt components and must not be added again.
			if t.Priced {
				total += t.Cost
				ok = true
			}
			continue
		}
		if t.Priced {
			total += t.Cost
			ok = true
		}
		if t.PlanPriced {
			total += t.PlanCost
			ok = true
		}
		if t.ReviewPriced {
			total += t.ReviewCost
			ok = true
		}
	}
	return total, ok
}

// SweepCost sums the USD cost of every priced trial across the whole report —
// the bottom-line "what did this run cost". ok=false when nothing was priced.
func (r *Report) SweepCost() (total float64, ok bool) {
	for _, c := range r.Cells {
		if t, has := c.CostTotal(); has {
			total += t
			ok = true
		}
	}
	return total, ok
}

// LadderN counts trials in the cell that have ladder metadata (i.e. were
// run by a ladder arm). 0 for non-ladder cells.
func (c Cell) LadderN() int {
	n := 0
	for _, t := range c.Trials {
		if t.Ladder != nil {
			n++
		}
	}
	return n
}

// LadderEscalations counts ladder trials that escalated past rung 1.
func (c Cell) LadderEscalations() int {
	n := 0
	for _, t := range c.Trials {
		if t.Ladder != nil && t.Ladder.Escalated {
			n++
		}
	}
	return n
}

// LadderEscalationRate is escalations / ladder-trials. 0 when no ladder trials.
func (c Cell) LadderEscalationRate() float64 {
	ln := c.LadderN()
	if ln == 0 {
		return 0
	}
	return float64(c.LadderEscalations()) / float64(ln)
}

// LadderMeanAttempts returns the mean number of attempts across ladder trials. 0
// when no ladder trials.
func (c Cell) LadderMeanAttempts() float64 {
	ln := c.LadderN()
	if ln == 0 {
		return 0
	}
	sum := 0
	for _, t := range c.Trials {
		if t.Ladder != nil {
			sum += t.Ladder.Attempts
		}
	}
	return float64(sum) / float64(ln)
}

// ReviewStatuses aggregates the status values of every trial's ReviewReport
// into a count histogram. Returns nil when no trial carries a review report
// (gate was off for the whole cell). Ladder trials only contribute the
// top-level trial Review; per-attempt review reports are not carried on
// AttemptRecord, so the histogram aggregates top-level review statuses.
func (c Cell) ReviewStatuses() map[string]int {
	var m map[string]int
	for _, t := range c.Trials {
		if t.Review == nil {
			continue
		}
		if m == nil {
			m = make(map[string]int)
		}
		m[string(t.Review.Status)]++
	}
	return m
}

// LadderCostByModel aggregates CostByModel across all ladder trials in this
// cell. Unpriced models are absent from the map. Returns nil when there are no
// ladder trials or no per-model data.
func (c Cell) LadderCostByModel() map[string]float64 {
	m := make(map[string]float64)
	for _, t := range c.Trials {
		if t.Ladder == nil {
			continue
		}
		for model, cost := range t.Ladder.CostByModel {
			m[model] += cost
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// LadderTouchRateByModel returns, for each model, the fraction of ladder trials
// in this cell where that model had nonzero cost.  Returns nil when there are
// no ladder trials.
func (c Cell) LadderTouchRateByModel() map[string]float64 {
	ln := c.LadderN()
	if ln == 0 {
		return nil
	}
	m := make(map[string]float64)
	for _, t := range c.Trials {
		if t.Ladder == nil {
			continue
		}
		for model, cost := range t.Ladder.CostByModel {
			if cost > 0 {
				m[model]++
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	for model := range m {
		m[model] /= float64(ln)
	}
	return m
}

// PricingCoverage classifies the cell by how completely its trials are priced:
// "full" (every trial has all applicable components priced), "partial" (some
// component priced, some not), "none" (nothing priced). For ladder trials the
// Ladder.Priced flag is the single all-in gate — ladder.Cost already includes
// solver + reviewer + planner.
func (c Cell) PricingCoverage() string {
	if c.N() == 0 {
		return "none"
	}
	allFull := true
	anyPriced := false
	for _, t := range c.Trials {
		full, priced := trialPricingCoverage(t)
		if priced {
			anyPriced = true
		}
		if !full {
			allFull = false
		}
	}
	if !anyPriced {
		return "none"
	}
	if allFull {
		return "full"
	}
	return "partial"
}

// trialPricingCoverage returns (fullyPriced, anyPriced) for a single trial.
func trialPricingCoverage(t Trial) (full, priced bool) {
	if t.Ladder != nil {
		return t.Ladder.Priced, t.Ladder.Priced
	}
	// Non-ladder: solver always applicable; plan/review applicable when non-nil.
	solverPriced := t.Priced
	planApplicable := t.Plan != nil
	reviewApplicable := t.Review != nil
	planOK := !planApplicable || t.PlanPriced
	reviewOK := !reviewApplicable || t.ReviewPriced

	priced = solverPriced || t.PlanPriced || t.ReviewPriced
	full = solverPriced && planOK && reviewOK
	return full, priced
}

// UnpricedModels returns the deduplicated set of model ids that were seen in
// this cell but not priced. For non-ladder trials each unpriced component
// contributes its model id (solver, planner, reviewer). For ladder trials only
// the top-level Priced flag is available — individual models inside the ladder
// cannot be distinguished — so the arm label itself is reported when unpriced.
func (c Cell) UnpricedModels() []string {
	seen := map[string]struct{}{}
	for _, t := range c.Trials {
		if t.Ladder != nil {
			if !t.Ladder.Priced {
				seen[c.Model] = struct{}{}
			}
			continue
		}
		if !t.Priced {
			seen[c.Model] = struct{}{}
		}
		if t.Plan != nil && !t.PlanPriced {
			seen[t.Plan.Model] = struct{}{}
		}
		if t.Review != nil && !t.ReviewPriced {
			seen[t.Review.ReviewerModel] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Manifest is the reproducibility record — pinned ids and settings so a report
// is a comparable artifact, not a one-off. The CLI fills it (it owns time, git,
// and the env), keeping this package pure and testable.
type Manifest struct {
	SchemaVersion  string   `json:"schema_version"` // bump when Trial/Cell/Manifest shape changes, so a diff tool can refuse to compare incompatible reports instead of misreading absent-vs-zero fields.
	GeneratedAt    string   `json:"generated_at"`
	GitSHA         string   `json:"git_sha"`
	GoVersion      string   `json:"go_version"`
	TrialsPer      int      `json:"trials_per_cell"`
	Models         []string `json:"models"`
	Cases          []string `json:"cases"`
	TaskSteerArmed bool     `json:"task_steer_armed,omitempty"`
}

// ReportSchemaVersion is the current eval-report schema. v2 added LatencyMs on
// Trial and the reason/latency columns; v3 added Trial.NoAttempt (Grade.
// NoAttempt) and the best-of-N selection section; v4 added infra tracking
// and plan-stage cost; v5 added ladder metadata and escalation guard metrics;
// v6 added task-steer arm metadata.
// Bump on any further shape change.
const ReportSchemaVersion = "6"

// Report is the aggregate of a sweep: the manifest plus every cell's trials. It
// renders to Markdown (human) and JSON (machine-diffable, for regression diffs).
type Report struct {
	Manifest Manifest `json:"manifest"`
	Cells    []Cell   `json:"cells"`
}

// Markdown renders the human-facing report: a manifest header, then one table
// per case with a row per model.
func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# Eval report\n\n")
	m := r.Manifest
	fmt.Fprintf(&b, "- generated: %s\n- git: %s\n- go: %s\n- trials/cell: %d\n",
		nonEmpty(m.GeneratedAt, "n/a"), nonEmpty(m.GitSHA, "n/a"), nonEmpty(m.GoVersion, "n/a"), m.TrialsPer)
	if m.TaskSteerArmed {
		b.WriteString("- arm: +steer\n")
	}
	b.WriteByte('\n')

	// Group cells by case, in first-seen order, for one table each.
	order := []string{}
	byCase := map[string][]Cell{}
	for _, c := range r.Cells {
		if _, seen := byCase[c.Case]; !seen {
			order = append(order, c.Case)
		}
		byCase[c.Case] = append(byCase[c.Case], c)
	}

	for _, name := range order {
		fmt.Fprintf(&b, "## %s\n\n", name)

		// Determine whether any cell in this case group carries ladder data —
		// if so, extend the table with escalation and attempts columns.
		hasLadder := false
		for _, c := range byCase[name] {
			if c.LadderN() > 0 {
				hasLadder = true
				break
			}
		}
		// Determine whether any cell has a review report — if so, extend
		// the table with a review statuses column.
		hasReview := false
		for _, c := range byCase[name] {
			if c.ReviewStatuses() != nil {
				hasReview = true
				break
			}
		}

		// Header.
		b.WriteString("| model | pass-rate | (ex-infra) | 95% CI | infra | false-pos | outcomes | iters p50/p90 (pass) | prompt tok p50 | reason tok p50 | latency p50 | $/trial p50 | pricing |")
		if hasLadder {
			b.WriteString(" escalation | $ by model | attempts mean |")
		}
		if hasReview {
			b.WriteString(" review statuses |")
		}
		b.WriteByte('\n')

		// Separator.
		b.WriteString("|-------|-----------|------------|--------|-------|-----------|----------|----------------------|----------------|----------------|-------------|-------------|---------|")
		if hasLadder {
			b.WriteString("------------|------------|---------------|")
		}
		if hasReview {
			b.WriteString("-----------------|")
		}
		b.WriteByte('\n')

		for _, c := range byCase[name] {
			lo, hi := c.PassRateCI()
			fp := ""
			if c.FalsePositives() > 0 {
				fp = " ⚠"
			}
			infra := ""
			if c.Infra() > 0 {
				infra = fmt.Sprintf("%d", c.Infra())
			}
			fmt.Fprintf(&b, "| %s | %d/%d (%.2f) | %.2f | [%.2f, %.2f] | %s | %d/%d%s | %s | %d/%d | %s | %s | %s | %s | %s |",
				c.Model, c.Passes(), c.N(), c.PassRate(), c.PassRateInfraExcluded(), lo, hi, infra,
				c.FalsePositives(), c.N(), fp,
				renderOutcomes(c.Outcomes()),
				c.ItersP(50, true), c.ItersP(90, true),
				humanInt(c.PromptTokensP(50)),
				humanInt(c.ReasoningTokensP(50)),
				humanMs(c.LatencyP(50)),
				renderCostP(c),
				renderPricingCoverage(c))
			if hasLadder {
				b.WriteString(renderLadderEscalation(c))
				b.WriteString(renderLadderCostByModel(c))
				fmt.Fprintf(&b, " %.2f |", c.LadderMeanAttempts())
			}
			if hasReview {
				b.WriteString(renderReviewStatuses(c.ReviewStatuses()))
			}
			b.WriteByte('\n')
		}
		b.WriteString("\n")
	}
	b.WriteString(bestOfMarkdown(r.Cells))
	b.WriteString(reviewGateMarkdown(r.Cells))
	if total, ok := r.SweepCost(); ok {
		fmt.Fprintf(&b, "_sweep cost: %s (priced models only; unpriced rows show —)_\n", humanUSD(total))
	}
	return b.String()
}

// WriteFiles archives the report to dir: report.md (human) and report.json
// (machine-diffable, the input to a future regression diff). The dir is created
// if absent.
func (r *Report) WriteFiles(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.Markdown()), 0o644); err != nil {
		return err
	}
	// Populate additive per-cell fields before JSON serialisation.
	for i := range r.Cells {
		r.Cells[i].PricingCoverage_ = r.Cells[i].PricingCoverage()
		r.Cells[i].UnpricedModels_ = r.Cells[i].UnpricedModels()
	}
	j, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), j, 0o644); err != nil {
		return err
	}
	return r.writeTraces(dir)
}

// writeTraces archives each trial's full think->act->observe trace to its own
// file under dir/traces/, kept OUT of report.json (Trial.Steps is json:"-") so
// the aggregate stays compact while a failed real-task cell is still replayable.
// A trial with no captured steps (a provider error before the first turn) is
// skipped — there is nothing to replay. Best-effort per file: one unwritable
// trace must not fail the whole report it accompanies.
func (r *Report) writeTraces(dir string) error {
	var traceDir string
	for _, c := range r.Cells {
		for _, t := range c.Trials {
			if len(t.Steps) == 0 {
				continue
			}
			if traceDir == "" {
				traceDir = filepath.Join(dir, "traces")
				if err := os.MkdirAll(traceDir, 0o755); err != nil {
					return err
				}
			}
			rec := traceRecord{Case: t.Case, Model: t.Model, Index: t.Index,
				Outcome: string(t.Outcome), Pass: t.Pass, Detail: t.Detail, Answer: t.Answer,
				Review: t.Review, Steps: t.Steps}
			b, err := json.MarshalIndent(rec, "", "  ")
			if err != nil {
				continue
			}
			name := fmt.Sprintf("%s__%s__t%d.json", t.Case, slugify(t.Model), t.Index)
			_ = os.WriteFile(filepath.Join(traceDir, name), b, 0o644)
		}
	}
	return nil
}

// traceRecord is the on-disk shape of one archived trial trace: enough header to
// identify the run (model/case/index + the verdict) followed by the full Step
// trace, so a trace file is self-describing without the report beside it. Answer
// is the final artifact lifted out of the trace — redundant with the last step's
// reply, but for a PROSE case (issue-review) it is the whole point of the run, so
// it sits at the top of the file where a reader looks first.
type traceRecord struct {
	Case    string              `json:"case"`
	Model   string              `json:"model"`
	Index   int                 `json:"index"`
	Outcome string              `json:"outcome"`
	Pass    bool                `json:"pass"`
	Detail  string              `json:"detail"`
	Answer  string              `json:"answer"`
	Review  *agent.ReviewReport `json:"review,omitempty"`
	Steps   []agent.Step        `json:"steps"`
}

// slugify makes a model slug safe for a filename: "/" and ":" — the only
// separators OpenRouter slugs use — become "_" (e.g. moonshotai/kimi-k2.6:free
// -> moonshotai_kimi-k2.6_free).
func slugify(s string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(s)
}

// ---- small helpers ----

// wilson is the Wilson score interval for a binomial proportion at 95% (z=1.96).
// It beats the naive normal interval at small N and near 0/1, which is exactly
// the regime an eval lives in (n=5..20, rates near the extremes).
func wilson(passes, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.96
	nf := float64(n)
	phat := float64(passes) / nf
	denom := 1 + z*z/nf
	center := (phat + z*z/(2*nf)) / denom
	margin := (z * math.Sqrt(phat*(1-phat)/nf+z*z/(4*nf*nf))) / denom
	return clamp01(center - margin), clamp01(center + margin)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// percentileInt returns the pth percentile (0..100) using nearest-rank on a
// sorted copy. Empty input is 0.
func percentileInt(xs []int, p float64) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

// percentileInt64 is percentileInt for millisecond durations (int64). Kept
// separate so latency math never narrows through int on a 32-bit build.
func percentileInt64(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

// percentileFloat is percentileInt for USD costs — nearest-rank on a sorted
// copy, empty input is 0. Kept separate from percentileInt so neither pays a
// conversion cost or loses precision.
func percentileFloat(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}

// renderCostP renders a cell's p50 per-trial cost, or "—" when the model is
// unpriced (CostP ok=false) — never a misleading $0.
func renderCostP(c Cell) string {
	cost, ok := c.CostP(50)
	if !ok {
		return "—"
	}
	return humanUSD(cost)
}

// renderPricingCoverage renders the cell's pricing coverage status for the
// Markdown table: "full", "partial", or "none". When not full, unpriced model
// ids are listed parenthetically so the reader sees WHAT is unpriced.
func renderPricingCoverage(c Cell) string {
	status := c.PricingCoverage()
	unpriced := c.UnpricedModels()
	if status == "full" || len(unpriced) == 0 {
		return status
	}
	return fmt.Sprintf("%s (%s)", status, strings.Join(unpriced, ", "))
}

// renderLadderEscalation returns a formatted escalation cell for the Markdown
// table: "n/N (rate)" for ladder cells, " — " for non-ladder cells.
func renderLadderEscalation(c Cell) string {
	ln := c.LadderN()
	if ln == 0 {
		return " — |"
	}
	return fmt.Sprintf(" %d/%d (%.2f) |", c.LadderEscalations(), ln, c.LadderEscalationRate())
}

// renderLadderCostByModel renders the "$ by model" column for ladder cells:
// " model $cost (share% · touch%) · …" for ladder cells, " — |" for non-ladder.
// Models are sorted by total cost descending so the dominant model is first.
func renderLadderCostByModel(c Cell) string {
	byModel := c.LadderCostByModel()
	if byModel == nil || len(byModel) == 0 {
		return " — |"
	}
	touch := c.LadderTouchRateByModel()

	// Sort models by cost descending for stable, readable output.
	type kv struct {
		m string
		c float64
	}
	pairs := make([]kv, 0, len(byModel))
	for m, cost := range byModel {
		pairs = append(pairs, kv{m, cost})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].c != pairs[j].c {
			return pairs[i].c > pairs[j].c
		}
		return pairs[i].m < pairs[j].m
	})

	// Compute total for share percentages.
	var total float64
	for _, p := range pairs {
		total += p.c
	}

	parts := make([]string, len(pairs))
	for i, p := range pairs {
		share := 0.0
		if total > 0 {
			share = p.c / total * 100
		}
		touchPct := 0.0
		if touch != nil {
			touchPct = touch[p.m] * 100
		}
		parts[i] = fmt.Sprintf("%s %s (%.0f%% · %.0f%%)",
			p.m, humanUSD(p.c), share, touchPct)
	}
	return " " + strings.Join(parts, " · ") + " |"
}

// humanUSD formats a dollar amount with enough precision to be useful at eval
// scale: sub-cent runs (a $0.0014 open model) need four decimals, dollar-plus
// totals read fine at two.
func humanUSD(d float64) string {
	if d > 0 && d < 0.1 {
		return fmt.Sprintf("$%.4f", d)
	}
	return fmt.Sprintf("$%.2f", d)
}

// renderOutcomes prints the histogram as "answered=4 hit_cap=1", outcomes in a
// stable (count-desc, then name) order so two reports diff cleanly.
func renderOutcomes(h map[agent.Outcome]int) string {
	type kv struct {
		k agent.Outcome
		v int
	}
	pairs := make([]kv, 0, len(h))
	for k, v := range h {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s=%d", p.k, p.v)
	}
	return strings.Join(parts, " ")
}

// renderReviewStatuses prints the review status histogram as "clean=4
// parse_error=1" with a trailing pipe, or "— |" when the map is nil (gate was
// off). Statuses are ordered count-desc then name for stable, diffable reports.
func renderReviewStatuses(m map[string]int) string {
	if m == nil {
		return " — |"
	}
	if len(m) == 0 {
		return " — |"
	}
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s=%d", p.k, p.v)
	}
	return " " + strings.Join(parts, " ") + " |"
}

func humanInt(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// humanMs renders a millisecond duration compactly: "850ms", "12.3s", "1.5m".
// 0 (no timing captured) renders as "—" so a pre-timing replay is not read as
// an instant run.
func humanMs(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%.1fm", float64(ms)/60000)
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
