package agent

import (
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestSpendPricesPerModelAndSums(t *testing.T) {
	s := NewSpend(func(model string, u llm.Usage) (float64, bool) {
		switch model {
		case "solver":
			return float64(u.TotalTokens) * 0.001, true
		case "reviewer":
			return float64(u.TotalTokens) * 0.01, true
		default:
			return 0, false
		}
	})

	s.Add("solver", llm.Usage{TotalTokens: 10})
	s.Add("reviewer", llm.Usage{TotalTokens: 20})

	got, ok := s.USD()
	want := 0.21
	if !ok || got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("USD() = %v, %v; want %v, true", got, ok, want)
	}
}

func TestSpendNilSafe(t *testing.T) {
	var s *Spend
	s.Add("model", llm.Usage{TotalTokens: 1})
	got, ok := s.USD()
	if got != 0 || ok {
		t.Fatalf("nil USD() = %v, %v; want 0, false", got, ok)
	}
}

func TestSpendEmptyUsageNoop(t *testing.T) {
	s := NewSpend(func(string, llm.Usage) (float64, bool) { return 1, true })
	s.Add("model", llm.Usage{TotalTokens: 5})
	before, ok := s.USD()
	if !ok {
		t.Fatal("setup Add should price")
	}

	s.Add("model", llm.Usage{})
	after, ok := s.USD()
	if !ok || after != before {
		t.Fatalf("after empty Add USD() = %v, %v; want %v, true", after, ok, before)
	}
}

func TestSpendUnpricedMakesUSDFailOpen(t *testing.T) {
	s := NewSpend(func(model string, u llm.Usage) (float64, bool) {
		if model == "priced" {
			return float64(u.TotalTokens), true
		}
		return 0, false
	})

	s.Add("priced", llm.Usage{TotalTokens: 2})
	s.Add("unknown", llm.Usage{TotalTokens: 3})

	got, ok := s.USD()
	if got != 2 || ok {
		t.Fatalf("USD() after unpriced Add = %v, %v; want 2, false", got, ok)
	}
}
