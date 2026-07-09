package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The delegation ledger: one JSONL record per finished run, appended by the
// binary so CLI and TUI turns land in the same file. ledger.sh consumes these
// field names, so null/omitempty semantics are intentionally pinned.
type ledgerRecord struct {
	TS           string   `json:"ts"`
	Event        string   `json:"event"`
	ID           *string  `json:"id"`
	Repo         string   `json:"repo"`
	Model        *string  `json:"model"`
	ReviewModel  *string  `json:"review_model"`
	Outcome      *string  `json:"outcome"`
	RescuedFrom  *string  `json:"rescued_from,omitempty"`
	ExitCode     int      `json:"exit_code"`
	Iterations   *int     `json:"iterations"`
	TotalTokens  *int     `json:"total_tokens"`
	SolverCost   *float64 `json:"solver_cost_usd"`
	ReviewerCost *float64 `json:"reviewer_cost_usd"`
	PlannerCost  *float64 `json:"planner_cost_usd"`
	SelectorCost *float64 `json:"selector_cost_usd"`
	TotalCost    *float64 `json:"total_cost_usd"`
	CostSource   *string  `json:"cost_source"`
	ReviewRounds *int     `json:"review_rounds"`
	ReviewBlock  *bool    `json:"review_blocked"`
	HasPatch     bool     `json:"has_patch"`
	Task         string   `json:"task"`
}

func LedgerDir() (string, error) {
	if dir := os.Getenv("DRIVER_OS_LEDGER_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "driver-os"), nil
}

type LedgerMeta struct {
	Repo        string
	ExitCode    int
	ReviewModel string
	HasPatch    bool
}

func AppendLedger(rec RunRecord, meta LedgerMeta) error {
	dir, err := LedgerDir()
	if err != nil {
		return fmt.Errorf("resolving ledger dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lr := ledgerRecord{
		TS:           time.Now().Format(time.RFC3339),
		Event:        "run",
		Repo:         meta.Repo,
		ExitCode:     meta.ExitCode,
		SolverCost:   rec.SolverCost,
		ReviewerCost: rec.ReviewerCost,
		PlannerCost:  rec.PlannerCost,
		SelectorCost: rec.SelectorCost,
		TotalCost:    rec.TotalCost,
		CostSource:   rec.CostSource,
		HasPatch:     meta.HasPatch,
		Task:         truncateRunes(rec.Task, 200),
	}
	if rec.ID != "" {
		lr.ID = &rec.ID
	}
	if rec.Model != "" {
		lr.Model = &rec.Model
	}
	if rec.Outcome != "" {
		o := string(rec.Outcome)
		lr.Outcome = &o
	}
	if rec.RescuedFrom != "" {
		rf := string(rec.RescuedFrom)
		lr.RescuedFrom = &rf
	}
	if meta.ReviewModel != "" {
		lr.ReviewModel = &meta.ReviewModel
	}
	iters := rec.Iterations
	lr.Iterations = &iters
	total := rec.Usage.TotalTokens
	lr.TotalTokens = &total
	if rec.Review != nil {
		lr.ReviewRounds = &rec.Review.Rounds
		lr.ReviewBlock = &rec.Review.Blocked
	}
	b, err := json.Marshal(lr)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.Write(append(b, '\n'))
	return err
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
