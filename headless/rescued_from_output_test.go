package headless

// CLI surfacing of the cap-rescue label (harness audit 2026-07-07, open item
// 4, second half): RunResult.RescuedFrom must reach every machine consumer —
// the result JSON, the report projection, and the delegation ledger — so a
// cap-rescued exit-0 is distinguishable from a model-claimed answer without
// parsing prose.

import (
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
)

func TestBuildResult_RescuedFrom(t *testing.T) {
	res := baseResult(agent.Answered)
	res.RescuedFrom = agent.HitCap
	r := buildResult(res, "m", "", "", nil)
	if r.RescuedFrom != "hit_cap" {
		t.Fatalf("RescuedFrom = %q, want %q", r.RescuedFrom, "hit_cap")
	}
	if r.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 — the label distinguishes, it does not reclassify", r.ExitCode)
	}
	if r2 := buildResult(baseResult(agent.Answered), "m", "", "", nil); r2.RescuedFrom != "" {
		t.Fatalf("RescuedFrom = %q on a non-rescued answer, want empty", r2.RescuedFrom)
	}
}

func TestResultProjection_CarriesRescuedFrom(t *testing.T) {
	res := baseResult(agent.Answered)
	res.RescuedFrom = agent.HitBudget
	out := string(resultProjection(buildResult(res, "m", "", "", nil)))
	if !strings.Contains(out, `"rescued_from": "hit_budget"`) {
		t.Fatalf("report projection missing rescued_from:\n%s", out)
	}
}
