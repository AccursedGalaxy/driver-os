package eval

// Review-gate calibration aggregation (REVIEW-GATE-PLAN slice 3): finding
// fates roll up ACROSS runs, keyed by reviewer model, so the reviewer's
// false-positive rate is a measured number instead of a vibe. The inputs are
// the run transcripts (agent.RunRecord.Review) the harness already persists.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AccursedGalaxy/driver-os/agent"
)

// ReviewFateStats aggregates one reviewer model's finding fates.
type ReviewFateStats struct {
	Runs     int // runs whose report named this reviewer.
	Rounds   int // total review rounds spent.
	Repaired int // blocked, fed back, cleared on a later round.
	Refuted  int // repro PASSED — the claim was execution-refuted.
	Expired  int // still blocking when rounds/run ran out.
	Notes    int // never blocked (severity note / under the confidence gate).
	Dropped  int // failed deterministic validation (quote/fence).
}

// Blockers is how many findings ever stood as (or were escalated toward)
// blocking: the denominator of the plan's FP formula.
func (s ReviewFateStats) Blockers() int { return s.Repaired + s.Refuted + s.Expired }

// FPRate is the plan's calibration metric: refuted+expired / total blockers
// (refuted = provably wrong; expired = never converted into a fix — the
// pessimistic proxy until human labels exist). NaN-free: 0 when no blockers.
func (s ReviewFateStats) FPRate() float64 {
	if s.Blockers() == 0 {
		return 0
	}
	return float64(s.Refuted+s.Expired) / float64(s.Blockers())
}

// AggregateReviewFates folds every record's review report into per-reviewer
// stats. Records without a report (gate off) are skipped; a report without a
// reviewer model aggregates under "(unknown)".
func AggregateReviewFates(recs []agent.RunRecord) map[string]*ReviewFateStats {
	out := map[string]*ReviewFateStats{}
	for _, rec := range recs {
		rep := rec.Review
		if rep == nil {
			continue
		}
		key := rep.ReviewerModel
		if key == "" {
			key = "(unknown)"
		}
		s := out[key]
		if s == nil {
			s = &ReviewFateStats{}
			out[key] = s
		}
		s.Runs++
		s.Rounds += rep.Rounds
		for _, f := range rep.Findings {
			switch f.Fate {
			case agent.FateRepaired:
				s.Repaired++
			case agent.FateRefuted:
				s.Refuted++
			case agent.FateExpired:
				s.Expired++
			case agent.FateNote:
				s.Notes++
			case agent.FateDropped:
				s.Dropped++
			}
		}
	}
	return out
}

// RenderReviewFates renders the aggregate as a stable, scannable table (one
// line per reviewer, name-sorted) for reports and the CLI.
func RenderReviewFates(stats map[string]*ReviewFateStats) string {
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		s := stats[n]
		fmt.Fprintf(&b, "%s: runs=%d rounds=%d blockers=%d (repaired=%d refuted=%d expired=%d) notes=%d dropped=%d fp_rate=%.2f\n",
			n, s.Runs, s.Rounds, s.Blockers(), s.Repaired, s.Refuted, s.Expired, s.Notes, s.Dropped, s.FPRate())
	}
	return strings.TrimRight(b.String(), "\n")
}
