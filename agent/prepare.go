package agent

// S6d.1 — the resolve-and-record seam (PROFILES.md §7.5).
//
// Prepare is the one place a RequestedConfig becomes a runnable spec. It
// resolves exactly once, keeps the trace that resolution produced, and packs
// the recording identity CENTRALLY — surfaces supply only what is genuinely
// surface-specific (RecordInputs) and never their own provenance view.
//
// Why the seam exists: every flag layer used to call profile.Resolve itself and
// hand a pre-resolved Config to Config.Split(), whose Requested() withholds the
// profile from resolution. The emitted ConfigRecord therefore carried TWO
// disagreeing provenance views of one run — a profile-blind runspec trace, and a
// FieldProvenance map the surface filled from its own profile.Resolve. Prepare
// collapses them: the trace it keeps IS the resolution that ran, and RecordMeta
// is derived from it.
//
// PRESENCE CONTRACT (load-bearing — read before writing a builder).
// The RequestedConfig handed to Prepare must express presence the way the flag
// layer does: for the profile-covered fields (profile.ProfileFields — the set
// overridesFrom projects) presence means THE FLAG WAS EXPLICITLY SET
// (fs.Visit), not value != zero. Prepare forwards the exec profile to
// resolution, so a nil profile-covered field takes the PROFILE's default. Feed
// it a request whose absent-ness merely means "zero" — as Config.Requested()
// produces — and an explicit -auto-verify=false collapses to nil and the
// profile's true is resurrected on top of it. That is precisely why
// Config.Requested() withholds the profile, and it is why Config is NOT a legal
// input to Prepare. Config/Split remain for the un-migrated surfaces until
// S6d.7 deletes them.
//
// Prepared is unforgeable on purpose: its fields are unexported and Prepare is
// its only constructor, so once the loops take a Prepared (S6d.7 collapses
// RunPrepared/RunNativePrepared onto Run/RunNative) no caller can resolve
// privately and reach a loop unrecorded.

import (
	"context"

	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/runspec"
)

// RecordInputs is the surface-specific half of the recording identity: what only
// the invoking binary knows. Everything derivable from the resolution itself
// (trust profile, exec profile name, required trust, canonicality, per-field
// provenance) is derived by Prepare and must NOT be passed in — that derivation
// is the whole point of the seam.
type RecordInputs struct {
	BinaryLabel            string // legacy v1-v3 conflated label; new callers set BinaryIdentity.
	BinaryIdentity         string
	InvocationSurface      string
	RequestedProtocol      string
	ProtocolFallbackReason string
	CLIOverrides           []string
	ApprovalPolicyName     string
	ApprovalPolicyHash     string
}

// Prepared is a resolved, recordable run: the spec the loop executes, the trace
// that produced it, and the runtime/content bindings. Constructed only by
// Prepare.
type Prepared struct {
	spec    runspec.ResolvedSpec
	trace   runspec.Trace
	runtime Runtime
	content Content
}

func (p Prepared) Spec() runspec.ResolvedSpec { return p.spec }
func (p Prepared) Trace() runspec.Trace       { return p.trace }
func (p Prepared) Runtime() Runtime           { return p.runtime }
func (p Prepared) Content() Content           { return p.content }
func (p Prepared) ConfigSHA256() string       { return p.spec.ConfigSHA256() }

// Prepare resolves req exactly once and returns a runnable, recordable run.
// rt.Record is overwritten with the centrally-derived RecordMeta: a caller
// cannot smuggle in a provenance view that disagrees with the resolution.
func Prepare(req runspec.RequestedConfig, rt Runtime, content Content, meta RecordInputs) (Prepared, error) {
	spec, trace, err := runspec.Resolve(req)
	if err != nil {
		return Prepared{}, err
	}
	rt.Record = recordMetaFrom(trace, meta)
	return Prepared{spec: spec, trace: trace, runtime: rt, content: content}, nil
}

// recordMetaFrom derives the recording identity from the resolution that
// actually ran, keeping only the surface-specific fields from meta.
func recordMetaFrom(t runspec.Trace, meta RecordInputs) RecordMeta {
	rm := RecordMeta{
		BinaryLabel:            meta.BinaryLabel,
		BinaryIdentity:         meta.BinaryIdentity,
		InvocationSurface:      meta.InvocationSurface,
		RequestedProtocol:      meta.RequestedProtocol,
		ProtocolFallbackReason: meta.ProtocolFallbackReason,
		CLIOverrides:           append([]string(nil), meta.CLIOverrides...),
		ApprovalPolicyName:     meta.ApprovalPolicyName,
		ApprovalPolicyHash:     meta.ApprovalPolicyHash,
		TrustProfile:           t.TrustProfile,
		ExecProfileName:        t.ExecProfile,
	}
	if t.ExecProfile != "" {
		if e, err := profile.ExecByName(t.ExecProfile); err == nil {
			rm.ExecProfileHash = e.Hash()
		}
	}
	// Provenance, canonicality and required trust come from the profile
	// resolution embedded in the trace — never from the caller.
	pt := t.Profile
	rm.RequiredTrust = string(pt.ProfileRequiredTrust)
	rm.Canonical = pt.Canonical
	if len(pt.Fields) > 0 {
		rm.FieldProvenance = make(map[string]string, len(pt.Fields))
		for k, v := range pt.Fields {
			rm.FieldProvenance[k] = string(v.Source)
		}
	}
	return rm
}

// RunPrepared / RunNativePrepared are the Prepared-taking loop entry points.
// S6d.7 collapses them onto Run/RunNative once every producer is migrated and
// the bare-ResolvedSpec entries are deleted.
func RunPrepared(ctx context.Context, p Prepared) (*RunResult, error) {
	return Run(ctx, p.spec, p.runtime, p.content)
}

func RunNativePrepared(ctx context.Context, p Prepared) (*RunResult, error) {
	return RunNative(ctx, p.spec, p.runtime, p.content)
}
