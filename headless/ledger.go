package headless

import (
	"strings"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
)

// stampRoleModels records the armed role model ids on the result object so a
// consumer (and the ledger) never has to re-parse argv to learn which
// reviewer/planner/selector a run was configured with. Empty = role off = null.
func stampRoleModels(r *resultObject, reviewModel, planModel, selectModel string) {
	if s := strings.TrimSpace(reviewModel); s != "" {
		r.ReviewModel = &s
	}
	if s := strings.TrimSpace(planModel); s != "" {
		r.PlanModel = &s
	}
	if s := strings.TrimSpace(selectModel); s != "" {
		r.SelectModel = &s
	}
}

func ledgerRecordFromResult(r resultObject) agent.RunRecord {
	return agent.RunRecord{
		SchemaVersion: agent.TranscriptSchemaVersion,
		ID:            r.ID,
		Model:         r.Model,
		Task:          r.Task,
		Outcome:       agent.Outcome(r.Outcome),
		RescuedFrom:   agent.Outcome(r.RescuedFrom),
		Iterations:    r.Iterations,
		Usage: llm.Usage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			TotalTokens:      r.Usage.TotalTokens,
			CachedTokens:     r.Usage.CachedTokens,
			ReasoningTokens:  r.Usage.ReasoningTokens,
		},
		Review:       r.Review,
		Guarantees:   r.Guarantees,
		Plan:         r.Plan,
		SolverCost:   r.SolverCost,
		ReviewerCost: r.ReviewerCost,
		PlannerCost:  r.PlannerCost,
		SelectorCost: r.SelectorCost,
		TotalCost:    r.TotalCost,
		CostSource:   r.CostSource,
	}
}
