package agent

import (
	"encoding/json"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// TestGuaranteesTerminalPaths is the terminal-path matrix from CONTROL-PLANE
// O8. The folding rule is deliberately outcome-independent: evidence reports
// what ran, even when completion ended by a cap, detector, cancellation, or
// infrastructure failure.
func TestGuaranteesTerminalPaths(t *testing.T) {
	paths := []struct {
		name    string
		outcome Outcome
	}{
		{"answered", Answered},
		{"unverified", Unverified},
		{"hit-cap", HitCap},
		{"hit-budget", HitBudget},
		{"stuck", KilledStagnant},
		{"canceled", Canceled},
		{"provider-err", ProviderErr},
		{"setup-failure", RefusedUnsafe},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			log := &evidenceLog{verifyConfigured: true, verifyCommand: "go test ./...", isolation: EvidencePassed}
			got := finalizeGuarantees(log, path.outcome, nil)
			if got.Verification.Status == EvidenceAbsent {
				t.Fatal("configured verification was reported absent")
			}
			if got.Verification.Status != EvidenceSkipped || !hasDegradation(got, "verification", ReasonNotReached) {
				t.Fatalf("pre-gate terminal path: verification=%q degradations=%+v, want skipped/not-reached", got.Verification.Status, got.Degradations)
			}
		})
	}
}

func TestFinalizeGuaranteesFreshness(t *testing.T) {
	log := &evidenceLog{verifyConfigured: true, verifyCommand: "test", isolation: EvidencePassed, mutGen: 1}
	log.verify = append(log.verify, verifyEvidenceEvent{status: EvidencePassed, command: "test", gen: 0})
	got := finalizeGuarantees(log, Answered, nil)
	if got.Verification.Status != EvidenceDegraded || !hasDegradation(got, "verification", ReasonStaleEvidence) {
		t.Fatalf("post-verify mutation: verification=%q degradations=%+v", got.Verification.Status, got.Degradations)
	}
	if got.Verification.MutationGen != 0 || got.Verification.FinalGen != 1 {
		t.Fatalf("generation provenance = %d/%d, want 0/1", got.Verification.MutationGen, got.Verification.FinalGen)
	}
}

func TestAllowUnpricedSpendRecordsDegradation(t *testing.T) {
	log := &evidenceLog{costConfigured: true}
	cfg := Config{MaxTotalCostUSD: 1, AllowUnpricedSpend: true, Obs: nopObserver{}, evidence: log}
	noted := false
	if stop, reason := dollarBudgetStop(cfg, fakeReportedUsage(), 1, &noted); stop {
		t.Fatalf("AllowUnpricedSpend stopped: %s", reason)
	}
	got := finalizeGuarantees(log, HitCap, nil)
	if got.CostBound != EvidenceDegraded || !hasDegradation(got, "cost-bound", ReasonUnpricedSpend) {
		t.Fatalf("cost guarantee=%q degradations=%+v", got.CostBound, got.Degradations)
	}
}

func fakeReportedUsage() (u llm.Usage) { u.TotalTokens = 1; return u }

func TestGuaranteesJSONRoundTrip(t *testing.T) {
	want := RunRecord{SchemaVersion: TranscriptSchemaVersion, Guarantees: Guarantees{
		Verification: VerificationEvidence{Status: EvidencePassed, Command: "go test", Attempts: 2, MutationGen: 3, FinalGen: 3},
		Diff:         DiffEvidence{Status: EvidencePassed, FilesChanged: []string{"agent/evidence.go"}, CaptureGen: 3},
		Degradations: []Degradation{{Guarantee: "review", Reason: ReasonStaleEvidence, Detail: "changed later"}},
	}}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got RunRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Guarantees.Verification != want.Guarantees.Verification || len(got.Guarantees.Diff.FilesChanged) != 1 || !hasDegradation(got.Guarantees, "review", ReasonStaleEvidence) {
		t.Fatalf("round trip lost guarantees: %+v", got.Guarantees)
	}
}

func hasDegradation(g Guarantees, guarantee string, reason DegradationReason) bool {
	for _, d := range g.Degradations {
		if d.Guarantee == guarantee && d.Reason == reason {
			return true
		}
	}
	return false
}
