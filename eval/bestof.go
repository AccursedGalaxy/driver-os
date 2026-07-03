package eval

import (
	"fmt"
	"strings"

	"github.com/AccursedGalaxy/driver-os/agent"
)

// Best-of-N selection: pick ONE trial per cell by structure alone, with no
// access to the oracle. The design and its evidence live in docs/findings/BEST-OF-N.md —
// measured on the SWE-bench stride-30 sweep, this selector matched the oracle
// on every one-pass/one-fail split (12/12 across three models) at N=2, because
// cheap-model failures are overwhelmingly STRUCTURAL (a detector kill, a cap,
// an infra error, an empty attempt) and structure is visible in the outcome.
// Its measured limit: answered-vs-answered splits (both verify green to the
// agent, one wrong on held-back tests) carry no structural signal — that mass
// is the ceiling, and patch-consensus voting measurably ANTI-selects there
// (failures correlate), so no content-based tiebreak is attempted.

// structuralRank orders terminal states by how much finished work they imply:
// a verified answer beats running out of iterations with work on disk, which
// beats a detector kill; a run that left nothing gradable (Grade.NoAttempt)
// ranks below everything — an `answered` with an empty diff is a confident
// no-op, not an attempt (the measured sympy-18698 case). Infra failures
// (Trial.Err) rank absolute last.
func structuralRank(t Trial) int {
	if t.Err != "" {
		return 4
	}
	if t.NoAttempt {
		return 3
	}
	switch t.Outcome {
	case agent.Answered:
		return 0
	case agent.HitCap:
		return 1
	default:
		return 2
	}
}

// SelectBest returns the index into trials of the structurally-best trial,
// breaking rank ties by the earliest trial Index (deterministic, and no reason
// to prefer a later attempt). -1 when trials is empty.
func SelectBest(trials []Trial) int {
	best := -1
	for i, t := range trials {
		if best == -1 {
			best = i
			continue
		}
		b := trials[best]
		if r, br := structuralRank(t), structuralRank(b); r < br || (r == br && t.Index < b.Index) {
			best = i
		}
	}
	return best
}

// Best returns the cell's selected trial (see SelectBest). ok is false for an
// empty cell.
func (c Cell) Best() (Trial, bool) {
	i := SelectBest(c.Trials)
	if i < 0 {
		return Trial{}, false
	}
	return c.Trials[i], true
}

// Split reports whether the cell mixes passing and failing trials — the only
// cells where selection has any work to do, and the denominator of the
// selection-accuracy metric.
func (c Cell) Split() bool {
	return c.Passes() > 0 && c.Passes() < c.N()
}

// bestOfMarkdown renders the per-model best-of-N summary section: what the
// sweep's pass-rate becomes when each cell reports its SELECTED trial instead
// of averaging all trials, next to the oracle ceiling (a cell counts if ANY
// trial passed) and the selection accuracy on splits. Empty when no cell has
// more than one trial — best-of-1 is just the pass-rate.
func bestOfMarkdown(cells []Cell) string {
	multi := false
	for _, c := range cells {
		if c.N() > 1 {
			multi = true
			break
		}
	}
	if !multi {
		return ""
	}

	type agg struct {
		cells, selected, oracle, splits, splitsRight int
		trials, trialPasses                          int
	}
	order := []string{}
	byModel := map[string]*agg{}
	for _, c := range cells {
		a, seen := byModel[c.Model]
		if !seen {
			a = &agg{}
			byModel[c.Model] = a
			order = append(order, c.Model)
		}
		a.cells++
		a.trials += c.N()
		a.trialPasses += c.Passes()
		if best, ok := c.Best(); ok && best.Pass {
			a.selected++
		}
		if c.Passes() > 0 {
			a.oracle++
		}
		if c.Split() {
			a.splits++
			if best, ok := c.Best(); ok && best.Pass {
				a.splitsRight++
			}
		}
	}

	var b strings.Builder
	b.WriteString("## Best-of-N (structural selection — see docs/findings/BEST-OF-N.md)\n\n")
	b.WriteString("| model | single-trial | best-of selected | oracle (any pass) | selection on splits |\n")
	b.WriteString("|-------|--------------|------------------|-------------------|---------------------|\n")
	for _, m := range order {
		a := byModel[m]
		fmt.Fprintf(&b, "| %s | %d/%d (%.2f) | %d/%d (%.2f) | %d/%d (%.2f) | %d/%d |\n",
			m,
			a.trialPasses, a.trials, ratio(a.trialPasses, a.trials),
			a.selected, a.cells, ratio(a.selected, a.cells),
			a.oracle, a.cells, ratio(a.oracle, a.cells),
			a.splitsRight, a.splits)
	}
	b.WriteString("\n")
	return b.String()
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
