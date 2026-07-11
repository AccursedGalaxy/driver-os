package agent

import (
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Repro for the 2026-07-09 dogfood finding (gpt-5.6 backlog session): with a
// role-aware Spend armed, one unpriceable role call latches Spend.unpriced
// forever and USD() fails open for the REST OF THE RUN — even when the priced
// partial sum has already crossed the cap. A priced under-count >= cap proves
// true cost >= cap, so enforcing on it is safe (it can only fire later than
// truth, never earlier). Fail-open is only correct while NOTHING is priced.
func TestDollarBudgetPartialSpendStillEnforces(t *testing.T) {
	spend := NewSpend(func(model string, u llm.Usage) (float64, bool) {
		if model == "metered-model" {
			return float64(u.TotalTokens) * 0.01, true
		}
		return 0, false // table-unknown role model
	})
	cfg := Config{MaxTotalCostUSD: 0.05, Spend: spend, Obs: nopObserver{}}
	var missing bool

	spend.Add("metered-model", llm.Usage{TotalTokens: 10}) // $0.10: over cap on its own.
	spend.Add("unknown-model", llm.Usage{TotalTokens: 5})  // unpriceable — must not poison enforcement.

	stop, reason := dollarBudgetStopT(cfg, llm.Usage{}, 2, &missing)
	if !stop {
		t.Fatalf("priced partial sum $0.10 >= cap $0.05 did not stop (reason %q) — one unpriceable call disabled the whole budget", reason)
	}
	if !strings.Contains(reason, "hit dollar budget") {
		t.Fatalf("reason = %q, want dollar budget reason", reason)
	}
}

// Repro for the same session's turn-0 false alarm: the budget check runs
// before any usage has accumulated, and an empty Spend reports ok=false, so
// the run opens with "dollar budget ... configured but cost could not be
// priced; continuing without dollar enforcement" — misleading (there is
// nothing to enforce yet) — and latches the once-only note. An empty Spend
// with no reported usage must stay silent, and enforcement must still work
// on later turns once priced usage arrives.
func TestDollarBudgetEmptySpendStaysQuiet(t *testing.T) {
	spend := NewSpend(func(model string, u llm.Usage) (float64, bool) {
		return float64(u.TotalTokens) * 0.01, true
	})
	obs := &recordObserver{}
	cfg := Config{MaxTotalCostUSD: 0.05, Spend: spend, Obs: obs}
	var missing bool

	if stop, _ := dollarBudgetStopT(cfg, llm.Usage{}, 1, &missing); stop {
		t.Fatal("empty spend stopped the run")
	}
	for _, n := range obs.notes {
		if strings.Contains(n, "could not be priced") {
			t.Fatalf("turn-0 empty spend emitted the unpriceable-cost note: %q", n)
		}
	}

	spend.Add("metered-model", llm.Usage{TotalTokens: 10}) // $0.10: over cap.
	if stop, _ := dollarBudgetStopT(cfg, llm.Usage{}, 2, &missing); !stop {
		t.Fatal("priced spend over cap did not stop after the quiet turn-0 check")
	}
}
