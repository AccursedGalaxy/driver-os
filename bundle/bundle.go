// Package bundle implements the portable proof-bundle format (schema v2;
// v1 bundles still verify).
//
// A bundle proves the integrity of the artifacts it contains and records the
// harness's evidence and attestations. It does not prove that the configured
// verifier fully captures task correctness.
package bundle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const SchemaVersion = 2
const Limit = "This bundle proves artifact integrity and records verification evidence; it does not prove that the verifier fully captures task correctness."

type Component struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Kind   string `json:"kind"` // reproducible_evidence
}
type Attestations struct {
	ChangedFiles      []string `json:"changed_files,omitempty"`
	WorkspaceEffect   string   `json:"workspace_effect"`
	DiffStatus        string   `json:"diff_status"`
	ScopeStatus       string   `json:"scope_status"`
	TestFenceStatus   string   `json:"test_fence_status"`
	Isolation         string   `json:"isolation"`
	ObservedIsolation string   `json:"observed_isolation"`
	ObservedNetwork   *bool    `json:"observed_network"`
	TrustPosture      string   `json:"trust_posture"`
}
type Identity struct {
	ExecutionProfile      string `json:"execution_profile,omitempty"`
	ExecutionProfileHash  string `json:"execution_profile_hash,omitempty"`
	EffectiveConfigSHA256 string `json:"effective_config_sha256"`
	// ResolutionTraceSHA256 / RuntimeResolutionSHA256 extend bundle identity
	// with the resolution trace and the runtime-derivation event sequence
	// (PROFILES.md §7.3): two runs sharing the policy hash cannot execute
	// different derived verify commands invisibly.
	ResolutionTraceSHA256   string `json:"resolution_trace_sha256,omitempty"`
	RuntimeResolutionSHA256 string `json:"runtime_resolution_sha256,omitempty"`
	PromptSHA256            string `json:"prompt_sha256"`
	ToolSchemaSHA256        string `json:"tool_schema_sha256"`
	Solver                  string `json:"solver,omitempty"`
	Reviewer                string `json:"reviewer,omitempty"`
	Planner                 string `json:"planner,omitempty"`
	HarnessCommit           string `json:"harness_commit,omitempty"`
	HarnessDirty            bool   `json:"harness_dirty"`
	BuildIdentity           string `json:"build_identity,omitempty"`
}
type Cost struct {
	Usage    any      `json:"usage"`
	TotalUSD *float64 `json:"total_usd"`
	State    string   `json:"state"`
	Source   *string  `json:"source"`
}
type Signature struct {
	Status               string `json:"signature_status"`
	PublicKey            string `json:"public_key,omitempty"`
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
	Value                string `json:"value,omitempty"`
}
type Manifest struct {
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	Limit         string       `json:"limit"`
	Components    []Component  `json:"reproducible_evidence"`
	Attestations  Attestations `json:"producer_attestations"`
	Verification  any          `json:"verification_evidence"`
	Review        any          `json:"review_evidence"`
	Degradations  any          `json:"degradations"`
	Uncertainty   []string     `json:"uncertainty,omitempty"`
	Identity      Identity     `json:"identity"`
	Cost          Cost         `json:"cost"`
	Signature     Signature    `json:"signature"`
}
type Input struct {
	Manifest                  Manifest
	TranscriptPath, PatchPath string
	VerifierOutput            []byte
	ClosingVerifierOutput     []byte
	SigningKey                ed25519.PrivateKey
}
type Result struct{ Path, ManifestSHA256 string }

type VerificationResult struct {
	ManifestSHA256       string
	ReproducibleEvidence []string
	Attestations         Attestations
	SignatureStatus      string
}

func canonical(v any) ([]byte, error) { return json.Marshal(v) }
func digest(b []byte) string          { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func cleanName(s string) string       { return filepath.Base(s) }

func Create(parent string, in Input) (Result, error) {
	if in.Manifest.RunID == "" {
		return Result{}, errors.New("bundle: empty run id")
	}
	dir := filepath.Join(parent, in.Manifest.RunID+".bundle")
	tmp, err := os.MkdirTemp(parent, "."+in.Manifest.RunID+".bundle-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(tmp)
	add := func(name, source string, data []byte) error {
		if source != "" {
			var e error
			data, e = os.ReadFile(source)
			if e != nil {
				return fmt.Errorf("%s: %w", name, e)
			}
		}
		p := cleanName(name)
		if err := os.WriteFile(filepath.Join(tmp, p), data, 0600); err != nil {
			return err
		}
		in.Manifest.Components = append(in.Manifest.Components, Component{Name: name, Path: p, SHA256: digest(data), Kind: "reproducible_evidence"})
		return nil
	}
	if in.TranscriptPath == "" {
		return Result{}, errors.New("transcript component unavailable")
	}
	if err = add("transcript", in.TranscriptPath, nil); err != nil {
		return Result{}, err
	}
	if in.PatchPath != "" {
		if err = add("patch", in.PatchPath, nil); err != nil {
			return Result{}, err
		}
	}
	// Preserve the v1 baseline component semantics: this is the failing baseline
	// output, and is empty when that baseline was green.
	if err = add("verify-baseline-output", "", in.VerifierOutput); err != nil {
		return Result{}, err
	}
	if err = add("verify-closing-output", "", in.ClosingVerifierOutput); err != nil {
		return Result{}, err
	}
	sort.Slice(in.Manifest.Components, func(i, j int) bool { return in.Manifest.Components[i].Name < in.Manifest.Components[j].Name })
	in.Manifest.SchemaVersion = SchemaVersion
	in.Manifest.Limit = Limit
	in.Manifest.Signature = Signature{Status: "unsigned"}
	if len(in.SigningKey) > 0 {
		if len(in.SigningKey) != ed25519.PrivateKeySize {
			return Result{}, errors.New("invalid ed25519 private key length")
		}
		pub := in.SigningKey.Public().(ed25519.PublicKey)
		in.Manifest.Signature = Signature{Status: "signed", PublicKey: base64.StdEncoding.EncodeToString(pub), PublicKeyFingerprint: digest(pub)}
		signable, _ := canonical(in.Manifest)
		in.Manifest.Signature.Value = base64.StdEncoding.EncodeToString(ed25519.Sign(in.SigningKey, signable))
	}
	body, err := canonical(in.Manifest)
	if err != nil {
		return Result{}, err
	}
	mh := digest(body)
	if err = os.WriteFile(filepath.Join(tmp, "manifest.json"), append(body, '\n'), 0600); err != nil {
		return Result{}, err
	}
	if err = os.WriteFile(filepath.Join(tmp, "manifest.sha256"), []byte(mh+"  manifest.json\n"), 0600); err != nil {
		return Result{}, err
	}
	_ = os.RemoveAll(dir)
	if err = os.Rename(tmp, dir); err != nil {
		return Result{}, err
	}
	return Result{Path: dir, ManifestSHA256: mh}, nil
}

// Verify checks a bundle offline: manifest hash, per-component digests, and
// the embedded signature if present. It never executes anything recorded in
// the bundle.
func Verify(path string) (VerificationResult, error) {
	st, err := os.Stat(path)
	if err != nil {
		return VerificationResult{}, err
	}
	dir := path
	if !st.IsDir() {
		dir = filepath.Dir(path)
	}
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return VerificationResult{}, fmt.Errorf("manifest: %w", err)
	}
	body = bytes.TrimSuffix(body, []byte("\n"))
	mh := digest(body)
	sum, err := os.ReadFile(filepath.Join(dir, "manifest.sha256"))
	if err != nil {
		return VerificationResult{}, fmt.Errorf("manifest hash: %w", err)
	}
	fields := strings.Fields(string(sum))
	if len(fields) < 1 || fields[0] != mh {
		return VerificationResult{}, errors.New("manifest: sha256 mismatch")
	}
	var m Manifest
	if err = json.Unmarshal(body, &m); err != nil {
		return VerificationResult{}, fmt.Errorf("manifest: %w", err)
	}
	if m.SchemaVersion != 1 && m.SchemaVersion != SchemaVersion {
		return VerificationResult{}, fmt.Errorf("manifest: unsupported schema_version %d", m.SchemaVersion)
	}
	out := VerificationResult{ManifestSHA256: mh, Attestations: m.Attestations, SignatureStatus: m.Signature.Status}
	for _, c := range m.Components {
		if c.Path == "" || filepath.Base(c.Path) != c.Path {
			return out, fmt.Errorf("component %s: invalid path", c.Name)
		}
		b, e := os.ReadFile(filepath.Join(dir, c.Path))
		if e != nil {
			return out, fmt.Errorf("component %s: %w", c.Name, e)
		}
		if digest(b) != c.SHA256 {
			return out, fmt.Errorf("component %s: sha256 mismatch", c.Name)
		}
		out.ReproducibleEvidence = append(out.ReproducibleEvidence, c.Name)
	}
	if m.Signature.Status == "signed" {
		sig, e := base64.StdEncoding.DecodeString(m.Signature.Value)
		if e != nil {
			return out, errors.New("signature: invalid encoding")
		}
		pub, e := base64.StdEncoding.DecodeString(m.Signature.PublicKey)
		if e != nil || len(pub) != ed25519.PublicKeySize {
			return out, errors.New("signature: invalid public key")
		}
		if digest(pub) != m.Signature.PublicKeyFingerprint {
			return out, errors.New("signature: public key fingerprint mismatch")
		}
		// The signable bytes are the manifest as written WITHOUT the signature
		// value key (omitempty drops it when Value is ""). Re-marshaling the
		// decoded manifest cannot reproduce them (nested evidence decodes to maps
		// whose key order differs from the producer's typed structs), so
		// reconstruct them from the raw bytes: value is std base64 (never
		// JSON-escaped) and the last field of Signature, so removing the exact
		// `,"value":"<sig>"` fragment yields the producer's signable bytes.
		signable := bytes.Replace(body, []byte(`,"value":"`+m.Signature.Value+`"`), nil, 1)
		if !ed25519.Verify(pub, signable, sig) {
			return out, errors.New("signature: verification failed")
		}
	} else if m.Signature.Status != "unsigned" {
		return out, errors.New("signature: invalid status")
	}
	return out, nil
}

func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		b, e = hex.DecodeString(s)
	}
	if e != nil {
		return nil, e
	}
	if len(b) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(b), nil
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("ed25519 key must be a 32-byte seed or 64-byte private key")
	}
	return ed25519.PrivateKey(b), nil
}
