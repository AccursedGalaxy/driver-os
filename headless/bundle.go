package headless

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/bundle"
)

// attachProofBundle is best-effort by contract. Artifact I/O must never rewrite
// the run's outcome or exit code; failures are exposed as a typed degradation
// and bundle_warning instead.
func attachProofBundle(r *resultObject) {
	if r.TranscriptPath == nil || *r.TranscriptPath == "" {
		bundleFailure(r, "transcript unavailable (transcript persistence may be disabled)")
		return
	}
	cr := r.configRecord
	var id bundle.Identity
	trust := ""
	if cr != nil {
		id = bundle.Identity{EffectiveConfigSHA256: cr.ConfigSHA256, ResolutionTraceSHA256: cr.ResolutionTraceSHA256, RuntimeResolutionSHA256: cr.RuntimeResolutionSHA256, PromptSHA256: cr.PromptSHA256, ToolSchemaSHA256: cr.ToolSchemaSHA256, ExecutionProfileHash: cr.ExecProfileHash, HarnessCommit: cr.HarnessCommit, HarnessDirty: cr.HarnessDirty, BuildIdentity: cr.BinaryIdentity + "/" + cr.InvocationSurface, Solver: r.Model}
		if cr.ExecProfile != nil {
			id.ExecutionProfile = *cr.ExecProfile
		}
		if cr.TrustProfile != nil {
			trust = *cr.TrustProfile
		}
		id.Reviewer = cr.Bindings.ReviewerIdentity
		id.Planner = cr.Bindings.PlannerIdentity
		if cr.Effective != nil { // legacy v<=8 records
			id.Reviewer, id.Planner = cr.Effective.ReviewerIdentity, cr.Effective.PlannerIdentity
		}
	}
	patch := ""
	if r.PatchPath != nil {
		patch = *r.PatchPath
	}
	scope, fence := "passed", "passed"
	if r.Outcome == string(agent.ScopeViolation) {
		scope = "failed"
		fence = "failed"
	}
	uncertainty := []string{}
	for _, d := range r.Guarantees.Degradations {
		uncertainty = append(uncertainty, string(d.Reason))
	}
	state := "priced"
	if r.TotalCost == nil {
		state = "unpriced"
	}
	verification := any(r.Guarantees.Verification)
	closingOut := []byte(nil)
	if r.closingVerification != nil {
		verification = r.closingVerification
		closingOut = []byte(r.closingVerification.Output)
	}
	m := bundle.Manifest{RunID: r.ID, Components: nil, Attestations: bundle.Attestations{ChangedFiles: r.Guarantees.Diff.FilesChanged, WorkspaceEffect: string(r.Guarantees.Diff.WorkspaceEffect), DiffStatus: string(r.Guarantees.Diff.Status), ScopeStatus: scope, TestFenceStatus: fence, Isolation: string(r.Guarantees.Isolation), ObservedIsolation: r.Guarantees.ObservedIsolation, ObservedNetwork: r.Guarantees.ObservedNetwork, TrustPosture: trust}, Verification: verification, Review: r.Guarantees.Review, Degradations: r.Guarantees.Degradations, Uncertainty: uncertainty, Identity: id, Cost: bundle.Cost{Usage: r.Usage, TotalUSD: r.TotalCost, State: state, Source: r.CostSource}}
	key, err := bundle.ParsePrivateKey(os.Getenv("DRIVER_BUNDLE_SIGNING_KEY"))
	if err != nil {
		bundleFailure(r, "invalid DRIVER_BUNDLE_SIGNING_KEY: "+err.Error())
		return
	}
	out := []byte(r.VerifyBaselineOut)
	got, err := bundle.Create(filepath.Dir(*r.TranscriptPath), bundle.Input{Manifest: m, TranscriptPath: *r.TranscriptPath, PatchPath: patch, VerifierOutput: out, ClosingVerifierOutput: closingOut, SigningKey: key})
	if err != nil {
		bundleFailure(r, err.Error())
		return
	}
	r.BundlePath = &got.Path
	r.BundleManifestSHA256 = &got.ManifestSHA256
	r.BundleStatus = "complete"
}
func bundleFailure(r *resultObject, detail string) {
	r.BundleWarning = detail
	r.BundleStatus = "degraded"
	r.Guarantees.Degradations = append(r.Guarantees.Degradations, agent.Degradation{Guarantee: "proof-bundle", Reason: agent.ReasonArtifactWrite, Detail: detail})
	fmt.Fprintln(os.Stderr, "bundle: WARNING:", detail, "(run outcome is unaffected)")
}
