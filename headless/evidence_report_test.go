package headless

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
)

// This derives the guarantees block from a typed value rather than pinning a
// golden copied from live output.
func TestReportRendersTypedGuarantees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.md")
	r := resultObject{
		Outcome:  "unverified",
		ExitCode: 2,
		Guarantees: agent.Guarantees{
			Verification: agent.VerificationEvidence{Status: agent.EvidenceDegraded},
			Review:       agent.ReviewEvidence{Status: agent.EvidenceSkipped},
			CostBound:    agent.EvidencePassed,
			Isolation:    agent.EvidencePassed,
			Diff:         agent.DiffEvidence{Status: agent.EvidencePassed},
			Degradations: []agent.Degradation{{Guarantee: "verification", Reason: agent.ReasonStaleEvidence, Detail: "changed later"}},
		},
	}
	if err := writeReport(path, r, reportOpts{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, derived := range []string{"## guarantees", "verification=degraded", "review=skipped", "verification: stale-evidence (changed later)"} {
		if !strings.Contains(text, derived) {
			t.Errorf("report missing %q:\n%s", derived, text)
		}
	}
}
