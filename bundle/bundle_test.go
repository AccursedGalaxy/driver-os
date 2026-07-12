package bundle

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, signed bool) Result {
	t.Helper()
	d := t.TempDir()
	tr := filepath.Join(d, "run.json")
	p := filepath.Join(d, "run.patch")
	if err := os.WriteFile(tr, []byte("transcript"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("patch"), 0600); err != nil {
		t.Fatal(err)
	}
	var key ed25519.PrivateKey
	if signed {
		_, key, _ = ed25519.GenerateKey(rand.Reader)
	}
	r, err := Create(d, Input{Manifest: Manifest{RunID: "r1", Attestations: Attestations{WorkspaceEffect: "changed"}, Verification: map[string]any{"command": "true"}}, TranscriptPath: tr, PatchPath: p, VerifierOutput: []byte("ok"), ClosingVerifierOutput: []byte("closing ok"), SigningKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func flip(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 1
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestVerifierRejectsTamperedComponentClasses(t *testing.T) {
	for _, name := range []string{"patch", "transcript", "verify-baseline-output", "verify-closing-output"} {
		t.Run(name, func(t *testing.T) {
			r := fixture(t, false)
			flip(t, filepath.Join(r.Path, name))
			_, err := Verify(r.Path)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("error=%v, want component name", err)
			}
		})
	}
}
func TestVerifierRejectsTamperedManifest(t *testing.T) {
	r := fixture(t, false)
	flip(t, filepath.Join(r.Path, "manifest.json"))
	_, err := Verify(r.Path)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("error=%v", err)
	}
}
func TestVerifierRejectsSignatureTamper(t *testing.T) {
	r := fixture(t, true)
	path := filepath.Join(r.Path, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "\"value\":\"")
	if i < 0 {
		t.Fatal("signature value absent")
	}
	i += len("\"value\":\"")
	x := []byte(s)
	if x[i] == 'A' {
		x[i] = 'B'
	} else {
		x[i] = 'A'
	}
	if err = os.WriteFile(path, x, 0600); err != nil {
		t.Fatal(err)
	}
	trim := strings.TrimSuffix(string(x), "\n")
	h := sha256.Sum256([]byte(trim))
	if err = os.WriteFile(filepath.Join(r.Path, "manifest.sha256"), []byte(hex.EncodeToString(h[:])+"  manifest.json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Verify(r.Path)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("error=%v", err)
	}
}

// TestVerifyNeverExecutesRecordedCommand pins that verification is offline
// only: the recorded verifier command in the manifest is data, never executed.
func TestVerifyNeverExecutesRecordedCommand(t *testing.T) {
	r := fixture(t, false)
	marker := filepath.Join(r.Path, "ran")
	rewriteManifestCommand(t, r.Path, "touch "+marker)
	if _, err := Verify(r.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("recorded command unexpectedly ran")
	}
}

// rewriteManifestCommand swaps the recorded verification command and re-stamps
// the manifest hash so the bundle still verifies (unsigned fixtures only).
func rewriteManifestCommand(t *testing.T, dir, cmd string) {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), `"command":"true"`, `"command":"`+cmd+`"`, 1)
	if s == string(b) {
		t.Fatal("recorded command not found in manifest")
	}
	if err = os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	trim := strings.TrimSuffix(s, "\n")
	h := sha256.Sum256([]byte(trim))
	if err = os.WriteFile(filepath.Join(dir, "manifest.sha256"), []byte(hex.EncodeToString(h[:])+"  manifest.json\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifierAcceptsV1Bundle pins backward compatibility: a bundle written by
// the v1 producer (no verify-closing-output component, schema_version 1) must
// still verify offline under the v2 verifier.
func TestVerifierAcceptsV1Bundle(t *testing.T) {
	dir := t.TempDir() + "/r1.bundle"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) Component {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		return Component{Name: name, Path: name, SHA256: digest(data), Kind: "reproducible_evidence"}
	}
	m := Manifest{
		SchemaVersion: 1, RunID: "r1", Limit: Limit,
		Components: []Component{
			write("patch", []byte("diff")),
			write("transcript", []byte(`{"id":"r1"}`)),
			write("verify-baseline-output", nil),
		},
		Signature: Signature{Status: "unsigned"},
	}
	body, err := canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.sha256"), []byte(digest(body)+"  manifest.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(dir)
	if err != nil {
		t.Fatalf("v1 bundle must verify under the v2 verifier: %v", err)
	}
	if len(res.ReproducibleEvidence) != 3 {
		t.Fatalf("components = %v, want the three v1 components", res.ReproducibleEvidence)
	}
}
