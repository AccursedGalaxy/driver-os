package profile

import (
	"fmt"
	"time"
)

// TrustPosture is the already-normalized trust result supplied by the caller.
// Resolve deliberately does not select or derive trust itself.
type TrustPosture struct {
	Selected Trust
	Posture  Posture
	Surface  string
}

// Overrides contains the profile-resolved command-line settings. Nil means the
// flag was absent, which is distinct from explicitly selecting a zero value.
type Overrides struct {
	MaxIters          *int
	MaxTokens         *int
	FinishNudge       *int
	RunTimeout        *time.Duration
	Effort            *string
	Worktree          *string
	VerifyContinue    *bool
	AutoVerify        *bool
	AutoVerifySoft    *bool
	Memory            *bool
	BootContext       *bool
	StandingContext   *bool
	BatchReads        *bool
	RequireDiff       *bool
	ReadWindow        *int
	ReadOutline       *bool
	ChurnNudgeRuns    *int
	NavSpiralWindow   *int
	AnswerNudgeWindow *int
	// CLIOnly is carried through for consumers; it never changes canonicality.
	CLIOnly map[string]any
}
type Source string

const (
	SourceProfile Source = "profile"
	SourceCLI     Source = "cli"
	SourceTrust   Source = "trust"
)

type TraceEntry struct {
	Value  any
	Source Source
}
type Trace struct {
	Fields               map[string]TraceEntry
	Canonical            bool
	SelectedTrust        Trust
	ProfileRequiredTrust Trust
}

// Normalized is the complete profile-resolved result. CLIOnly is deliberately
// separate because it is not descriptor content.
type Normalized struct {
	Exec
	CLIOnly map[string]any
}

func v1Declarations(e Exec) map[FieldID]Declaration {
	m := map[FieldID]Declaration{}
	for _, f := range ProfileFields {
		var v any
		switch f {
		case MaxItersField:
			v = e.MaxIters
		case MaxTokensField:
			v = e.MaxTokens
		case FinishNudgeField:
			v = e.FinishNudge
		case RunTimeoutField:
			v = e.RunTimeout
		case EffortField:
			v = e.Effort
		case WorktreeField:
			v = e.Worktree
		case VerifyContinueField:
			v = e.VerifyContinue
		case AutoVerifyField:
			v = e.AutoVerify
		case MemoryField:
			v = e.Memory
		case BootContextField:
			v = e.BootContext
		case StandingContextField:
			v = e.StandingContext
		case BatchReadsField:
			v = e.BatchReads
		case AutoVerifySoftField:
			v = false
		case RequireDiffField:
			v = false
		case ReadWindowField:
			v = 0
		case ReadOutlineField:
			v = false
		case ChurnNudgeRunsField:
			v = 0
		case NavSpiralWindowField:
			v = 4
		case AnswerNudgeWindowField:
			v = 0
		}
		m[f] = Declaration{v, Default}
	}
	return m
}
func overrideFor(o Overrides, f FieldID) any {
	switch f {
	case MaxItersField:
		return o.MaxIters
	case MaxTokensField:
		return o.MaxTokens
	case FinishNudgeField:
		return o.FinishNudge
	case RunTimeoutField:
		return o.RunTimeout
	case EffortField:
		return o.Effort
	case WorktreeField:
		return o.Worktree
	case VerifyContinueField:
		return o.VerifyContinue
	case AutoVerifyField:
		return o.AutoVerify
	case AutoVerifySoftField:
		return o.AutoVerifySoft
	case MemoryField:
		return o.Memory
	case BootContextField:
		return o.BootContext
	case StandingContextField:
		return o.StandingContext
	case BatchReadsField:
		return o.BatchReads
	case RequireDiffField:
		return o.RequireDiff
	case ReadWindowField:
		return o.ReadWindow
	case ReadOutlineField:
		return o.ReadOutline
	case ChurnNudgeRunsField:
		return o.ChurnNudgeRuns
	case NavSpiralWindowField:
		return o.NavSpiralWindow
	case AnswerNudgeWindowField:
		return o.AnswerNudgeWindow
	}
	return nil
}
func pointed(v any) (any, bool) {
	switch x := v.(type) {
	case *int:
		if x != nil {
			return *x, true
		}
	case *bool:
		if x != nil {
			return *x, true
		}
	case *string:
		if x != nil {
			return *x, true
		}
	case *time.Duration:
		if x != nil {
			return *x, true
		}
	}
	return nil, false
}
func worktreeValue(s string) Worktree {
	if s == "required" {
		return WorktreeRequired
	}
	return WorktreeAuto
}

// Resolve merges immutable profile declarations, explicit flags, and the
// supplied trust posture. It is pure: no environment, flags, or repository IO.
func Resolve(post TrustPosture, e Exec, ov Overrides) (Normalized, Trace, error) {
	if err := RequireTrust(e, post.Selected); err != nil {
		return Normalized{}, Trace{}, err
	}
	ds := declarations[e.Name]
	if ds == nil {
		ds = v1Declarations(e)
	}
	n := Normalized{Exec: e, CLIOnly: ov.CLIOnly}
	tr := Trace{Fields: map[string]TraceEntry{}, Canonical: true, SelectedTrust: post.Selected, ProfileRequiredTrust: e.RequiredTrust}
	for _, f := range ProfileFields {
		d, ok := ds[f]
		if !ok {
			return n, tr, fmt.Errorf("profile %q omits field %s", e.Name, f)
		}
		v := d.Value
		src := SourceProfile
		if x, set := pointed(overrideFor(ov, f)); set {
			v = x
			src = SourceCLI
			tr.Canonical = false
		}
		setExec(&n.Exec, f, v)
		tr.Fields[string(f)] = TraceEntry{v, src}
	}
	for k, v := range ov.CLIOnly {
		tr.Fields[k] = TraceEntry{v, SourceCLI}
	}
	// Worktree is presently the only execution field constrained by posture.
	floor := post.Posture.Worktree
	got := worktreeValue(n.Worktree)
	if !got.Satisfies(floor) {
		entry := tr.Fields[string(WorktreeField)]
		d := ds[WorktreeField]
		if entry.Source == SourceCLI {
			return n, tr, fmt.Errorf("worktree conflicts with trust floor: cli value %q", n.Worktree)
		}
		if d.Stance == Demand {
			return n, tr, fmt.Errorf("worktree conflicts with trust floor: profile demand %q", n.Worktree)
		}
		n.Worktree = "required"
		tr.Fields[string(WorktreeField)] = TraceEntry{"required", SourceTrust}
	}
	return n, tr, nil
}
