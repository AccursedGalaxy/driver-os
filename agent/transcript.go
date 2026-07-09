package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// TranscriptSchemaVersion is the on-disk RunRecord schema. Bump on any shape
// change so a reader can refuse an incompatible record instead of misreading
// absent-vs-zero fields.
//
//	v1 = original; Usage had no json tags → PascalCase keys on disk.
//	v2 = Usage carries snake_case json tags (llm.Usage.UnmarshalJSON dual-accepts
//	     both shapes for backward-compat with v1-era records).
//	v3 = RunRecord carries Messages, the continuation seam for resuming a
//	     conversation into agent.Session.
//	v4 = RunRecord carries RescuedFrom, preserving the pre-upgrade outcome for
//	     cap/kill/deadline/budget runs rescued by harness gates.
//	v5 = RunRecord carries per-role cost fields shared with the delegation ledger.
const TranscriptSchemaVersion = "5"

// newRunID is "<YYYYMMDD-HHMMSS>-<hex>" — sortable by time, unique by suffix.
// Mirrors the council recorder's scheme so the two corpora read alike.
func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// runIndexFile is the longitudinal index filename WriteTranscript appends to.
const runIndexFile = "runs.jsonl"

// transcriptArtifactRE matches a per-run transcript filename ("<id>.json", id
// from newRunID: 8 date digits, 6 time digits, 8 hex chars). Kept next to
// newRunID so the pattern can't drift from the format it mirrors.
var transcriptArtifactRE = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{8}\.json$`)

// isTranscriptArtifact reports whether name is one of the harness's OWN
// run-transcript files — a per-run "<id>.json" or the "runs.jsonl" index. When a
// caller points TranscriptDir at (or under) the sandbox workspace, these files
// land in the agent's CWD; list_dir excludes them so the agent can't discover
// and read its own transcript — which derailed a dogfood run (it read the
// transcript, concluded it was looping, and bailed). Belt-and-suspenders: keep
// artifacts out of the sandbox root AND defensively hide ours if they leak in.
func isTranscriptArtifact(name string) bool {
	return name == runIndexFile || transcriptArtifactRE.MatchString(name)
}

// stampRun writes the run identity + wall-clock bounds onto a result from a
// deferred exit in Run/RunNative. nil-safe (a pre-run refusal may return nil),
// and it never overwrites an ID/StartedAt already set, so it is idempotent if a
// caller ever wraps one loop in another.
func stampRun(res *RunResult, id string, started time.Time) {
	if res == nil {
		return
	}
	if res.ID == "" {
		res.ID = id
	}
	if res.StartedAt.IsZero() {
		res.StartedAt = started
	}
	res.EndedAt = time.Now()
}

// RunRecord is the durable, self-describing envelope for one agent run — the P1
// spine every consumer shares (a CLI transcript, an eval Trial, a council
// AgentTrace, a commit-msg dogfood record). It carries the full Step trace so a
// run is replayable from the file alone, plus enough header (id, model, timing,
// outcome, usage) to be read without it.
type RunRecord struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"id"`
	StartedAt     string    `json:"started_at,omitempty"`
	EndedAt       string    `json:"ended_at,omitempty"`
	Model         string    `json:"model,omitempty"` // the caller's provider label (the loop doesn't know it).
	Task          string    `json:"task"`
	Outcome       Outcome   `json:"outcome"`
	RescuedFrom   Outcome   `json:"rescued_from,omitempty"`
	Answer        string    `json:"answer,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Iterations    int       `json:"iterations"`
	Usage         llm.Usage `json:"usage"`
	Err           string    `json:"error,omitempty"`
	// Review is the review-gate record (rounds, findings + fates, reviewer
	// usage) — the calibration telemetry the corpus tooling aggregates
	// (FP rate per reviewer = refuted+expired / total blockers). Absent when
	// the run had no Reviewer configured.
	Review *ReviewReport `json:"review,omitempty"`
	// Plan is the plan-stage record (the plan handed to the solver, or why
	// there was none, plus the planner's own usage — kept out of Usage so
	// per-role spend stays attributable). Absent when the run had no Planner.
	Plan *PlanReport `json:"plan,omitempty"`

	SolverCost   *float64 `json:"solver_cost_usd,omitempty"`
	ReviewerCost *float64 `json:"reviewer_cost_usd,omitempty"`
	PlannerCost  *float64 `json:"planner_cost_usd,omitempty"`
	SelectorCost *float64 `json:"selector_cost_usd,omitempty"`
	TotalCost    *float64 `json:"total_cost_usd,omitempty"`
	CostSource   *string  `json:"cost_source,omitempty"`

	Messages []llm.Message `json:"messages,omitempty"`
	Steps    []Step        `json:"steps,omitempty"`
}

// RecordFrom builds a RunRecord from a finished run. model is the caller's label
// for the provider (a slug like "openai/gpt-5.5"); pass "" when unknown.
func RecordFrom(res *RunResult, model string) RunRecord {
	if res == nil {
		return RunRecord{SchemaVersion: TranscriptSchemaVersion}
	}
	rec := RunRecord{
		SchemaVersion: TranscriptSchemaVersion,
		ID:            res.ID,
		Model:         model,
		Task:          res.Task,
		Outcome:       res.Outcome,
		RescuedFrom:   res.RescuedFrom,
		Answer:        res.Answer,
		Reason:        res.Reason,
		Iterations:    res.Iterations,
		Usage:         res.Usage,
		Review:        res.Review,
		Plan:          res.Plan,
		SolverCost:    res.SolverCost,
		ReviewerCost:  res.ReviewerCost,
		PlannerCost:   res.PlannerCost,
		SelectorCost:  res.SelectorCost,
		TotalCost:     res.TotalCost,
		CostSource:    res.CostSource,
		Messages:      res.Messages,
		Steps:         res.Steps,
	}
	if !res.StartedAt.IsZero() {
		rec.StartedAt = res.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if !res.EndedAt.IsZero() {
		rec.EndedAt = res.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	if res.Err != nil {
		rec.Err = res.Err.Error()
	}
	return rec
}

// TranscriptDir is the default run-transcript root, honoring $XDG_STATE_HOME
// (run history is STATE, not the council DATA corpus) and falling back to
// ~/.local/state. Override with $DRIVER_OS_TRANSCRIPT_DIR.
//
//	$XDG_STATE_HOME/driver-os/runs/   (default ~/.local/state/driver-os/runs/)
func TranscriptDir() string {
	if d := os.Getenv("DRIVER_OS_TRANSCRIPT_DIR"); d != "" {
		return d
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ".driver-os-runs" // no home and no XDG: last-resort relative dir, never panic.
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "driver-os", "runs")
}

// WriteTranscript persists a run two ways under dir: the full record as
// "<id>.json" (replayable on its own), and a one-line summary appended to
// "runs.jsonl" (the longitudinal index — every run this machine made, newest
// last, cheap to tail or scan for correlation/dedup). dir is created if absent.
// Returns the per-run file path.
func WriteTranscript(dir string, rec RunRecord) (string, error) {
	if rec.ID == "" {
		return "", errors.New("transcript: record has no run ID")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	full, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, rec.ID+".json")
	if err := os.WriteFile(path, append(full, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := appendRunIndex(filepath.Join(dir, runIndexFile), rec); err != nil {
		return path, err // the full record landed; surface the index failure but don't lose the path.
	}
	return path, nil
}

// LoadTranscript reads one per-run transcript by run id from dir.
func LoadTranscript(dir, runID string) (*RunRecord, error) {
	if runID == "" {
		return nil, errors.New("transcript: empty run ID")
	}
	raw, err := os.ReadFile(filepath.Join(dir, runID+".json"))
	if err != nil {
		return nil, err
	}
	var rec RunRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// runIndexEntry is the compact, trace-less summary line in runs.jsonl. The task
// is previewed (first line, clipped) so a giant inlined-diff task can't bloat the
// index; the full task lives in the per-run file.
type runIndexEntry struct {
	ID          string    `json:"id"`
	EndedAt     string    `json:"ended_at,omitempty"`
	Model       string    `json:"model,omitempty"`
	Outcome     Outcome   `json:"outcome"`
	RescuedFrom Outcome   `json:"rescued_from,omitempty"`
	Iterations  int       `json:"iterations"`
	Usage       llm.Usage `json:"usage"`
	TaskPreview string    `json:"task_preview,omitempty"`
}

func appendRunIndex(path string, rec RunRecord) error {
	line, err := json.Marshal(runIndexEntry{
		ID: rec.ID, EndedAt: rec.EndedAt, Model: rec.Model, Outcome: rec.Outcome, RescuedFrom: rec.RescuedFrom,
		Iterations: rec.Iterations, Usage: rec.Usage, TaskPreview: taskPreview(rec.Task),
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// taskPreview clips a task to its first line and ~120 runes for the index.
func taskPreview(task string) string {
	s := task
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 120 {
		return string(r[:117]) + "..."
	}
	return s
}
