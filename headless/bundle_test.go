package headless

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/bundle"
)

func TestAttachProofBundleFailureIsVisibleAndDoesNotChangeOutcome(t *testing.T) {
	r := resultObject{Outcome: string(agent.Answered), ExitCode: 0}
	attachProofBundle(&r)
	if r.Outcome != string(agent.Answered) || r.ExitCode != 0 {
		t.Fatalf("bundle failure changed result: %+v", r)
	}
	if r.BundleWarning == "" || r.BundleStatus != "degraded" || len(r.Guarantees.Degradations) != 1 || r.Guarantees.Degradations[0].Reason != agent.ReasonArtifactWrite {
		t.Fatalf("bundle failure not typed and visible: %+v", r)
	}
}

func TestAttachProofBundleSuccessSetsCompleteAndClosingEvidence(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "run1.json")
	if err := os.WriteFile(tr, []byte(`{"id":"run1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := resultObject{
		ID: "run1", Outcome: string(agent.Answered), TranscriptPath: &tr,
		closingVerification: &agent.VerificationRecord{
			Status: agent.EvidencePassed, Command: "go test ./...",
			Output: "ok", Tree: "tree-abc", Attempts: 1,
		},
	}
	attachProofBundle(&r)
	if r.BundleWarning != "" || r.BundleStatus != "complete" || r.BundlePath == nil {
		t.Fatalf("success path not marked complete: %+v", r)
	}
	res, err := bundle.Verify(*r.BundlePath, false)
	if err != nil {
		t.Fatalf("fresh bundle failed offline verification: %v", err)
	}
	found := false
	for _, name := range res.ReproducibleEvidence {
		if name == "verify-closing-output" {
			found = true
		}
	}
	if !found {
		t.Fatalf("verify-closing-output component missing: %v", res.ReproducibleEvidence)
	}
	// The manifest's verification evidence must carry the typed closing record.
	body, err := os.ReadFile(filepath.Join(*r.BundlePath, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Verification struct {
			Command string `json:"command"`
			Status  string `json:"status"`
			Tree    string `json:"tree"`
		} `json:"verification_evidence"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Verification.Command != "go test ./..." || m.Verification.Status != string(agent.EvidencePassed) || m.Verification.Tree != "tree-abc" {
		t.Fatalf("manifest verification evidence lacks the typed closing record: %+v", m.Verification)
	}
}
