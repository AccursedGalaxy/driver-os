package eval

import (
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
)

func tr(idx int, o agent.Outcome, pass, noAttempt bool) Trial {
	return Trial{Index: idx, Outcome: o, Pass: pass, NoAttempt: noAttempt}
}

func TestSelectBestStructuralRank(t *testing.T) {
	// Each case is a measured split shape from the SWE-bench stride-30 sweep
	// (BEST-OF-N.md): the selector must pick the passing trial on every
	// structural split — that's the 12/12 result the design rests on.
	cases := []struct {
		name   string
		trials []Trial
		want   int // index SelectBest must return
	}{
		{"answered beats hit_cap", // astropy-12907 gemini
			[]Trial{tr(1, agent.Answered, true, false), tr(2, agent.HitCap, false, false)}, 0},
		{"answered beats hit_cap reversed order", // matplotlib-25498 glm
			[]Trial{tr(1, agent.HitCap, false, false), tr(2, agent.Answered, true, false)}, 1},
		{"hit_cap beats killed_spiral", // django-12125 deepseek
			[]Trial{tr(1, agent.HitCap, true, false), tr(2, agent.KilledSpiral, false, false)}, 0},
		{"answered beats killed_spiral", // django-13230 glm
			[]Trial{tr(1, agent.Answered, true, false), tr(2, agent.KilledSpiral, false, false)}, 0},
		{"answered beats provider_error", // django-12708 gemini
			[]Trial{tr(1, agent.Answered, true, false), tr(2, agent.ProviderErr, false, false)}, 0},
		{"answered beats unverified", // sympy-18698 deepseek arms 1+2
			[]Trial{tr(1, agent.Unverified, false, false), tr(2, agent.Answered, true, false)}, 1},
		{"no-attempt answered ranks below real answered", // sympy-18698 with arm 3:
			// the empty-diff `answered` (Index 1, earliest!) must NOT win the
			// Index tiebreak against the answered trial that did work.
			[]Trial{tr(1, agent.Answered, false, true), tr(2, agent.Answered, true, false)}, 1},
		{"no-attempt ranks below hit_cap",
			[]Trial{tr(1, agent.Answered, false, true), tr(2, agent.HitCap, true, false)}, 1},
		{"rank tie breaks to earliest index",
			[]Trial{tr(2, agent.Answered, false, false), tr(1, agent.Answered, true, false)}, 1},
	}
	for _, c := range cases {
		if got := SelectBest(c.trials); got != c.want {
			t.Errorf("%s: SelectBest = %d, want %d", c.name, got, c.want)
		}
	}
	if got := SelectBest(nil); got != -1 {
		t.Errorf("SelectBest(nil) = %d, want -1", got)
	}
}

func TestSelectBestCannotSeeOracle(t *testing.T) {
	// The known ceiling (BEST-OF-N.md finding 2): answered-vs-answered splits
	// carry no structural signal, so the selector falls back to the earliest
	// index — it must NOT accidentally key on Pass (that would be peeking at
	// the held-out oracle the agent can never run).
	trials := []Trial{tr(1, agent.Answered, false, false), tr(2, agent.Answered, true, false)}
	if got := SelectBest(trials); got != 0 {
		t.Errorf("SelectBest = %d, want 0 — rank ties must resolve by index, never by the oracle verdict", got)
	}
}

func TestBestOfMarkdownSection(t *testing.T) {
	rep := &Report{Cells: []Cell{
		{Case: "a", Model: "m1", Trials: []Trial{ // split: selection has work to do
			tr(1, agent.Answered, true, false), tr(2, agent.KilledSpiral, false, false)}},
		{Case: "b", Model: "m1", Trials: []Trial{ // both fail: oracle ceiling excludes it
			tr(1, agent.Answered, false, false), tr(2, agent.HitCap, false, false)}},
	}}
	md := rep.Markdown()
	if !strings.Contains(md, "## Best-of-N") {
		t.Fatalf("multi-trial report must include the best-of section:\n%s", md)
	}
	// m1: single-trial 1/4, selected 1/2 cells, oracle 1/2, splits 1/1.
	if !strings.Contains(md, "| m1 | 1/4 (0.25) | 1/2 (0.50) | 1/2 (0.50) | 1/1 |") {
		t.Errorf("best-of row wrong:\n%s", md)
	}

	single := &Report{Cells: []Cell{{Case: "a", Model: "m1", Trials: []Trial{tr(1, agent.Answered, true, false)}}}}
	if strings.Contains(single.Markdown(), "## Best-of-N") {
		t.Error("n=1 report must omit the best-of section — best-of-1 is just the pass-rate")
	}
}
