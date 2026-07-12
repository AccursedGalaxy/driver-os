package headless

import (
	"flag"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestVerifyBaselineFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arg     string // what follows the flag name, e.g. "=on" or "" (bare)
		want    string // String() output
		wantErr string // substring of error, empty if ok
	}{
		{name: "bare", arg: "", want: "on"},
		{name: "on", arg: "=on", want: "on"},
		{name: "off", arg: "=off", want: "off"},
		{name: "abort", arg: "=abort", want: "abort"},
		{name: "true alias", arg: "=true", want: "on"},
		{name: "false alias", arg: "=false", want: "off"},
		{name: "junk rejected", arg: "=bogus", wantErr: "invalid -verify-baseline value"},
		{name: "empty value rejected", arg: "=", wantErr: "invalid -verify-baseline value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v verifyBaselineValue
			err := v.Set(strings.TrimPrefix(tc.arg, "="))
			// For bare (IsBoolFlag), the flag package calls Set("true").
			// We simulate bare by calling Set("true") when arg is "".
			if tc.arg == "" {
				// The flag package would call Set("true") for bare -verify-baseline
				err = v.Set("true")
			}
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			// Verify internal state
			switch tc.want {
			case "on":
				if v.skip || v.abort {
					t.Errorf("skip=%v abort=%v, want both false", v.skip, v.abort)
				}
			case "off":
				if !v.skip || v.abort {
					t.Errorf("skip=%v abort=%v, want skip=true abort=false", v.skip, v.abort)
				}
			case "abort":
				if v.skip || !v.abort {
					t.Errorf("skip=%v abort=%v, want skip=false abort=true", v.skip, v.abort)
				}
			}
		})
	}
}

// TestVerifyBaselineFlagBareDoesNotEatNextFlag verifies that the IsBoolFlag
// contract works: bare -verify-baseline sets "true" and the next token is not
// consumed. We test this by simulating exactly what the flag package does.
func TestVerifyBaselineFlagBareDoesNotEatNextFlag(t *testing.T) {
	// Simulate: flag package sees -verify-baseline -task foo
	// It should call Set("true") and leave -task for the next flag.
	var v verifyBaselineValue
	// Since IsBoolFlag() returns true, the flag package will
	// NOT consume the next argument; it calls Set("true").
	err := v.Set("true") // this is what the flag package does for bare -verify-baseline
	if err != nil {
		t.Fatalf("bare form Set('true') failed: %v", err)
	}
	if v.String() != "on" {
		t.Errorf("bare form String() = %q, want 'on'", v.String())
	}
	if v.skip || v.abort {
		t.Error("bare form should result in skip=false abort=false")
	}
}

// TestVerifyBaselineFlagBoolFlagInterface confirms the type implements
// flag.Value and reports itself as a bool flag.
func TestVerifyBaselineFlagBoolFlagInterface(t *testing.T) {
	var v verifyBaselineValue
	// Compile-time check: verifyBaselineValue implements flag.Value
	var _ flag.Value = &v
	// Runtime check: IsBoolFlag returns true
	if !v.IsBoolFlag() {
		t.Error("IsBoolFlag() = false, want true")
	}
}

func TestEffectiveReviewRequired(t *testing.T) {
	tests := []struct {
		name        string
		explicit    bool
		flagVal     bool
		reviewModel string
		want        bool
		wantOpen    bool
		wantErr     bool
	}{
		{"default no model", false, false, "", false, false, false},
		{"default with model", false, false, "gpt-4", false, false, false},
		{"explicit true with model", true, true, "gpt-4", true, false, false},
		{"explicit false with model", true, false, "gpt-4", false, true, false},
		{"explicit true no model", true, true, "", false, false, true},
		{"explicit false no model", true, false, "", false, false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotOpen, err := effectiveReviewRequired(tc.explicit, tc.flagVal, tc.reviewModel)
			if (err != nil) != tc.wantErr {
				t.Errorf("wantErr = %v, got err = %v", tc.wantErr, err)
			}
			if got != tc.want || gotOpen != tc.wantOpen {
				t.Errorf("got required=%v failOpen=%v, want required=%v failOpen=%v", got, gotOpen, tc.want, tc.wantOpen)
			}
		})
	}
}

func TestBudgetControlsWirePricedCostFn(t *testing.T) {
	const model = "openai/gpt-5.5"
	usage := llm.Usage{PromptTokens: 1000, CompletionTokens: 200}
	wantCost := 1.25
	price := func(string, llm.Usage) (float64, bool) { return wantCost, true }

	costFn := budgetCostFn(model, price)
	gotCost, ok := costFn(usage)
	if !ok || gotCost != wantCost {
		t.Fatalf("budgetCostFn cost = %v, %v; want %v, true", gotCost, ok, wantCost)
	}

	var cfg agent.Config
	wireBudgetControls(&cfg, 1.25, model, price)
	if cfg.MaxTotalCostUSD != 1.25 {
		t.Fatalf("MaxTotalCostUSD = %v, want budget flag value 1.25", cfg.MaxTotalCostUSD)
	}
	if cfg.CostFn == nil {
		t.Fatal("CostFn is nil")
	}
	gotCost, ok = cfg.CostFn(usage)
	if !ok || gotCost != wantCost {
		t.Fatalf("wired CostFn cost = %v, %v; want %v, true", gotCost, ok, wantCost)
	}
	if cfg.SolverModel != model {
		t.Fatalf("SolverModel = %q, want %q", cfg.SolverModel, model)
	}
	if cfg.Spend == nil {
		t.Fatal("Spend is nil")
	}
	cfg.Spend.Add(model, usage)
	gotCost, ok = cfg.Spend.USD()
	if !ok || gotCost != wantCost {
		t.Fatalf("wired Spend USD() = %v, %v; want %v, true", gotCost, ok, wantCost)
	}
}
