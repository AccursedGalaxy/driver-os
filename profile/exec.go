package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Exec is a versioned execution-profile descriptor. The original fields remain
// directly available for consumers of generation one profiles.
type Exec struct {
	Name            string
	RequiredTrust   Trust
	MaxIters        int
	MaxTokens       int
	FinishNudge     int
	RunTimeout      time.Duration
	Effort          string
	Worktree        string
	VerifyContinue  bool
	AutoVerify      bool
	Memory          bool
	BootContext     bool
	StandingContext bool
	BatchReads      bool
	// Fields added in generation two. They do not participate in v1 hashes.
	AutoVerifySoft    bool
	RequireDiff       bool
	ReadWindow        int
	ReadOutline       bool
	ChurnNudgeRuns    int
	NavSpiralWindow   int
	AnswerNudgeWindow int
}

type FieldID string
type Stance string

const (
	Default                Stance  = "default"
	Demand                 Stance  = "demand"
	MaxItersField          FieldID = "max_iters"
	MaxTokensField         FieldID = "max_tokens"
	FinishNudgeField       FieldID = "finish_nudge"
	RunTimeoutField        FieldID = "run_timeout"
	EffortField            FieldID = "effort"
	WorktreeField          FieldID = "worktree"
	VerifyContinueField    FieldID = "verify_continue"
	AutoVerifyField        FieldID = "auto_verify"
	AutoVerifySoftField    FieldID = "auto_verify_soft"
	MemoryField            FieldID = "memory"
	BootContextField       FieldID = "boot_context"
	StandingContextField   FieldID = "standing_context"
	BatchReadsField        FieldID = "batch_reads"
	RequireDiffField       FieldID = "require_diff"
	ReadWindowField        FieldID = "read_window"
	ReadOutlineField       FieldID = "read_outline"
	ChurnNudgeRunsField    FieldID = "churn_nudge_runs"
	NavSpiralWindowField   FieldID = "nav_spiral_window"
	AnswerNudgeWindowField FieldID = "answer_nudge_window"
)

var ProfileFields = []FieldID{MaxItersField, MaxTokensField, FinishNudgeField, RunTimeoutField, EffortField, WorktreeField, VerifyContinueField, AutoVerifyField, AutoVerifySoftField, MemoryField, BootContextField, StandingContextField, BatchReadsField, RequireDiffField, ReadWindowField, ReadOutlineField, ChurnNudgeRunsField, NavSpiralWindowField, AnswerNudgeWindowField}

type Declaration struct {
	Value  any    `json:"value"`
	Stance Stance `json:"stance"`
}

var execProfiles = map[string]Exec{
	"coding-v1":      {Name: "coding-v1", MaxIters: 40, MaxTokens: 8192, RunTimeout: 60 * time.Second, Effort: "low", Worktree: "auto", VerifyContinue: true, AutoVerify: true, Memory: true, BatchReads: true},
	"interactive-v1": {Name: "interactive-v1", MaxTokens: 32768, FinishNudge: 5, RunTimeout: 60 * time.Second, Worktree: "off", VerifyContinue: true, AutoVerify: true, Memory: true, StandingContext: true, BatchReads: true},
}
var declarations = map[string]map[FieldID]Declaration{}

func init() {
	add := func(name string, vals ...any) {
		m := map[FieldID]Declaration{}
		for i, f := range ProfileFields {
			m[f] = Declaration{Value: vals[i], Stance: Default}
		}
		declarations[name] = m
		e := Exec{Name: name}
		for f, d := range m {
			setExec(&e, f, d.Value)
		}
		execProfiles[name] = e
	}
	add("coding-v2", 40, 8192, 0, 60*time.Second, "low", "auto", true, true, false, true, false, false, true, true, 0, false, 0, 4, 0)
	add("interactive-v2", 0, 32768, 5, 60*time.Second, "", "off", true, true, false, true, false, true, true, false, 0, false, 0, 4, 0)
	add("observe-v1", 40, 8192, 5, 60*time.Second, "low", "auto", true, false, false, true, false, true, true, false, 0, false, 0, 4, 0)
	// eval-swe-v1 mirrors cmd/eval's registered flag defaults (max-iters 30,
	// max-tokens 4096, verify-continue false, batch-reads false, require-diff
	// false — trials are judged by the bench oracle, not a diff gate) so a
	// bare eval invocation is CANONICAL under this profile. Corrected from the
	// initial draft values in the same session that introduced the name,
	// before any transcript recorded it; the golden was re-pinned with this
	// justification and must not move again.
	add("eval-swe-v1", 30, 4096, 0, 60*time.Second, "", "off", false, false, false, false, false, false, false, false, 0, false, 0, 4, 0)
	d := declarations["eval-swe-v1"][WorktreeField]
	d.Stance = Demand
	declarations["eval-swe-v1"][WorktreeField] = d
}
func setExec(e *Exec, f FieldID, v any) {
	switch f {
	case MaxItersField:
		e.MaxIters = v.(int)
	case MaxTokensField:
		e.MaxTokens = v.(int)
	case FinishNudgeField:
		e.FinishNudge = v.(int)
	case RunTimeoutField:
		e.RunTimeout = v.(time.Duration)
	case EffortField:
		e.Effort = v.(string)
	case WorktreeField:
		e.Worktree = v.(string)
	case VerifyContinueField:
		e.VerifyContinue = v.(bool)
	case AutoVerifyField:
		e.AutoVerify = v.(bool)
	case AutoVerifySoftField:
		e.AutoVerifySoft = v.(bool)
	case MemoryField:
		e.Memory = v.(bool)
	case BootContextField:
		e.BootContext = v.(bool)
	case StandingContextField:
		e.StandingContext = v.(bool)
	case BatchReadsField:
		e.BatchReads = v.(bool)
	case RequireDiffField:
		e.RequireDiff = v.(bool)
	case ReadWindowField:
		e.ReadWindow = v.(int)
	case ReadOutlineField:
		e.ReadOutline = v.(bool)
	case ChurnNudgeRunsField:
		e.ChurnNudgeRuns = v.(int)
	case NavSpiralWindowField:
		e.NavSpiralWindow = v.(int)
	case AnswerNudgeWindowField:
		e.AnswerNudgeWindow = v.(int)
	}
}
func ExecByName(name string) (Exec, error) {
	e, ok := execProfiles[name]
	if !ok {
		return Exec{}, fmt.Errorf("unknown execution profile %q", name)
	}
	return e, nil
}
func (e Exec) Hash() string {
	var content any
	if declarations[e.Name] != nil {
		content = struct {
			Name          string                  `json:"name"`
			RequiredTrust Trust                   `json:"required_trust"`
			Fields        map[FieldID]Declaration `json:"fields"`
		}{e.Name, e.RequiredTrust, declarations[e.Name]}
	} else {
		content = struct {
			Name            string        `json:"name"`
			RequiredTrust   Trust         `json:"required_trust"`
			MaxIters        int           `json:"max_iters"`
			MaxTokens       int           `json:"max_tokens"`
			FinishNudge     int           `json:"finish_nudge"`
			RunTimeout      time.Duration `json:"run_timeout"`
			Effort          string        `json:"effort"`
			Worktree        string        `json:"worktree"`
			VerifyContinue  bool          `json:"verify_continue"`
			AutoVerify      bool          `json:"auto_verify"`
			Memory          bool          `json:"memory"`
			BootContext     bool          `json:"boot_context"`
			StandingContext bool          `json:"standing_context"`
			BatchReads      bool          `json:"batch_reads"`
		}{e.Name, e.RequiredTrust, e.MaxIters, e.MaxTokens, e.FinishNudge, e.RunTimeout, e.Effort, e.Worktree, e.VerifyContinue, e.AutoVerify, e.Memory, e.BootContext, e.StandingContext, e.BatchReads}
	}
	b, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func RequireTrust(e Exec, selected Trust) error {
	if e.RequiredTrust == "" {
		return nil
	}
	if _, err := ParseTrust(string(e.RequiredTrust)); err != nil {
		return fmt.Errorf("execution profile %q has invalid required trust: %w", e.Name, err)
	}
	if _, err := ParseTrust(string(selected)); err != nil {
		return err
	}
	candidate, floor := FloorFor(selected), FloorFor(e.RequiredTrust)
	for _, d := range descriptors {
		if !d.satisfies(candidate, floor) {
			return fmt.Errorf("execution profile %q requires trust %q: %s", e.Name, e.RequiredTrust, d.violation(candidate, floor))
		}
	}
	return nil
}
