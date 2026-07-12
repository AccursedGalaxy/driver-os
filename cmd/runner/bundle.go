package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/AccursedGalaxy/driver-os/bundle"
)

func bundleCommand(args []string) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(os.Stderr, "Usage: runner bundle verify [-rerun-verify] <bundle-dir|manifest.json>")
		return 2
	}
	fs := flag.NewFlagSet("runner bundle verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	rerun := fs.Bool("rerun-verify", false, "execute the recorded verifier command (off by default)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "bundle verify: exactly one path is required")
		return 2
	}
	r, err := bundle.Verify(fs.Arg(0), *rerun)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bundle verify:", err)
		return 1
	}
	out := struct {
		Valid                bool                `json:"valid"`
		ManifestSHA256       string              `json:"manifest_sha256"`
		ReproducibleEvidence []string            `json:"reproducible_evidence"`
		ProducerAttestations bundle.Attestations `json:"producer_attestations"`
		SignatureStatus      string              `json:"signature_status"`
		RerunOutput          string              `json:"rerun_output,omitempty"`
	}{true, r.ManifestSHA256, r.ReproducibleEvidence, r.Attestations, r.SignatureStatus, r.RerunOutput}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}
