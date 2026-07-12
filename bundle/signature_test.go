package bundle

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// A signed bundle whose Verification is a typed STRUCT (JSON keys in
// declaration order, not alphabetical) must verify cleanly.
func TestSignedBundleWithTypedEvidenceVerifies(t *testing.T) {
	d := t.TempDir()
	tr := filepath.Join(d, "run.json")
	if err := os.WriteFile(tr, []byte("transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	type typedEvidence struct {
		Status      string `json:"status"`
		Command     string `json:"command"`
		Attempts    int    `json:"attempts"`
		MutationGen int    `json:"mutation_gen"`
		FinalGen    int    `json:"final_gen"`
	}
	r, err := Create(d, Input{
		Manifest:       Manifest{RunID: "r1", Verification: typedEvidence{Status: "passed", Command: "go test ./...", Attempts: 1, MutationGen: 3, FinalGen: 3}},
		TranscriptPath: tr, VerifierOutput: []byte("ok"), SigningKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(r.Path, false); err != nil {
		t.Fatalf("clean signed bundle failed verification: %v", err)
	}
}
