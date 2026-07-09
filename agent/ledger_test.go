package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// TestAppendLedgerRecordShape pins the shared ledger emitter to the JSONL
// shape delegate.sh historically wrote and ledger.sh still parses (stats
// reads ts/event/id/model/outcome/total_cost_usd/has_patch; mark joins on
// id). One finished run = exactly ONE appended line.
func TestAppendLedgerRecordShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_OS_LEDGER_DIR", dir)

	solver := 0.10
	total := 0.12
	src := "metered"
	rec := RunRecord{
		SchemaVersion: TranscriptSchemaVersion,
		ID:            "run-1",
		Model:         "z-ai/glm-5.2",
		Task:          strings.Repeat("x", 300),
		Outcome:       Answered,
		Iterations:    7,
		Usage:         llm.Usage{TotalTokens: 4200},
		SolverCost:    &solver,
		TotalCost:     &total,
		CostSource:    &src,
		Review:        &ReviewReport{Rounds: 2, Blocked: false},
	}
	err := AppendLedger(rec, LedgerMeta{
		Repo:        "/repo/path",
		ExitCode:    0,
		ReviewModel: "openai/gpt-5.5",
		HasPatch:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("one run must append exactly one ledger line, got %d", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	for k, want := range map[string]any{
		"event": "run", "id": "run-1", "repo": "/repo/path",
		"model": "z-ai/glm-5.2", "review_model": "openai/gpt-5.5",
		"outcome": "answered", "exit_code": float64(0), "iterations": float64(7),
		"total_tokens": float64(4200), "solver_cost_usd": 0.10,
		"total_cost_usd": 0.12, "cost_source": "metered",
		"review_rounds": float64(2), "review_blocked": false, "has_patch": true,
	} {
		if m[k] != want {
			t.Errorf("record[%q] = %v, want %v", k, m[k], want)
		}
	}
	if ts, _ := m["ts"].(string); ts == "" {
		t.Error("record must carry a ts timestamp")
	}
	if task, _ := m["task"].(string); len([]rune(task)) != 200 {
		t.Errorf("task should be clipped to 200 runes, got %d", len([]rune(task)))
	}
	// Roles that never ran stay null, never zero (unknown ≠ free).
	for _, k := range []string{"reviewer_cost_usd", "planner_cost_usd", "selector_cost_usd"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s should be present and null, got %v (present=%v)", k, v, ok)
		}
	}
}

// TestAppendLedgerUnpricedCostsAreNull pins the null semantics for a run with
// NO cost information (e.g. an unpriceable model on the TUI path):
// total_cost_usd must be present and null — never 0. Unpriced ≠ free.
func TestAppendLedgerUnpricedCostsAreNull(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_OS_LEDGER_DIR", dir)

	rec := RunRecord{
		SchemaVersion: TranscriptSchemaVersion,
		ID:            "run-2",
		Model:         "some/unpriced-model",
		Task:          "do a thing",
		Outcome:       Answered,
		Iterations:    3,
		Usage:         llm.Usage{TotalTokens: 100},
	}
	if err := AppendLedger(rec, LedgerMeta{Repo: "/repo/path", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(string(data), "\n")), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"solver_cost_usd", "total_cost_usd", "cost_source"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s must be present and null (unpriced != free), got %v (present=%v)", k, v, ok)
		}
	}
	// No review model armed → review_model null, review_rounds null.
	if v, ok := m["review_model"]; !ok || v != nil {
		t.Errorf("review_model should be present and null, got %v (present=%v)", v, ok)
	}
}

// TestLedgerDirDefault pins the resolution order: $DRIVER_OS_LEDGER_DIR wins,
// else ~/.local/state/driver-os (the path ledger.sh reads).
func TestLedgerDirDefault(t *testing.T) {
	t.Setenv("DRIVER_OS_LEDGER_DIR", "/custom/dir")
	got, err := LedgerDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/dir" {
		t.Errorf("LedgerDir with env override = %q, want /custom/dir", got)
	}
	t.Setenv("DRIVER_OS_LEDGER_DIR", "")
	got, err = LedgerDir()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".local", "state", "driver-os"); got != want {
		t.Errorf("LedgerDir default = %q, want %q", got, want)
	}
}
