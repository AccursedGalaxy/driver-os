package headless

import (
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// A slim binary (empty Extras) must refuse capability flags with a setup
// error — never panic and never limp into a half-armed run. The ladder guard
// fires before any provider is constructed, so this exercises the real CLI
// path with no API key in the environment.
func TestNilExtrasLadderFlagFailsSetup(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	code := MainWithExtras([]string{"-ladder", "does-not-matter.json", "-task", "t", "-ledger=false"}, "test", Extras{})
	if code != 1 {
		t.Fatalf("-ladder with empty Extras: exit = %d, want 1 (setup error)", code)
	}
}

// priceBestOfReport is the modeled-cost fallback for the selector; a nil
// price func must degrade to unpriced (Cost stays nil), and a supplied one
// must populate Cost. Regression pin: the call moved from selectBestOf to the
// CLI boundary, where the injected PriceLookup lives — an unpriced call there
// would silently null every best-of selector cost.
func TestPriceBestOfReport(t *testing.T) {
	mk := func() *bestOfReport {
		return &bestOfReport{SelectorModel: "m", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10}}
	}

	r := mk()
	priceBestOfReport(r, nil)
	if r.Cost != nil {
		t.Fatalf("nil price: Cost = %v, want nil", *r.Cost)
	}

	r = mk()
	priceBestOfReport(r, func(model string, u llm.Usage) (float64, bool) {
		if !strings.EqualFold(model, "m") {
			return 0, false
		}
		return 0.42, true
	})
	if r.Cost == nil || *r.Cost != 0.42 {
		t.Fatalf("priced: Cost = %v, want 0.42", r.Cost)
	}

	// Metered usage cost wins over the modeled lookup.
	r = mk()
	r.Usage.Cost = 0.07
	priceBestOfReport(r, func(string, llm.Usage) (float64, bool) { return 0.42, true })
	if r.Cost == nil || *r.Cost != 0.07 {
		t.Fatalf("metered: Cost = %v, want 0.07", r.Cost)
	}
}
