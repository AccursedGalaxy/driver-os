package agent

import (
	"sync"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Spend accumulates a run's cumulative dollar cost across ROLES (solver,
// reviewer, planner), pricing each role's tokens at its OWN model instead of
// forcing one model's rate onto all of them. Shared through Config as a POINTER
// so a single accumulator survives the closing review gate, mid-loop repair
// rounds, and the best-of-N/repair path's fresh loop() invocations. Safe for
// concurrent Add.
type Spend struct {
	mu       sync.Mutex
	usd      float64
	priced   bool // at least one Add priced successfully
	unpriced bool // at least one Add could not be priced (unknown model)
	price    func(model string, u llm.Usage) (float64, bool)
}

// NewSpend builds an accumulator over a role-aware pricer. price should prefer a
// provider-reported per-call cost (Usage.Cost) when present and fall back to a
// static table, returning ok=false for an unpriceable model.
func NewSpend(price func(model string, u llm.Usage) (float64, bool)) *Spend {
	return &Spend{price: price}
}

// Add prices one role call's usage at its model and folds it in. Nil-safe and
// a no-op on empty usage, so callers Add unconditionally.
func (s *Spend) Add(model string, u llm.Usage) {
	if s == nil || s.price == nil || !usageReported(u) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.price(model, u)
	if !ok {
		s.unpriced = true
		return
	}
	s.usd += c
	s.priced = true
}

// USD reports cumulative dollars and whether the figure is COMPLETE. ok=false
// when nothing has been priced yet or some call could not be priced — the
// budget then FAILS OPEN (enforcing on an under-count could wrongly stop a run),
// matching dollarBudgetStop's existing conservative behavior.
func (s *Spend) USD() (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usd, s.priced && !s.unpriced
}
