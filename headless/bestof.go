// Best-of-N solving with comparative double-blind selection.
//
// Measured on SWE-bench Lite: cheap-solver pass@1 is 64% but oracle pass@3 is
// 77% — redundancy + selection is worth ~13 points. A comparative DOUBLE-BLIND
// reviewer picked the better of two patches 4/5 with zero order-flips
// (~$0.13/pair), while ABSOLUTE structural selection anti-selects at N>=3.
//
// The structural-rank tiebreak is a replica of eval.structuralRank +
// eval.SelectBest (eval/bestof.go), kept here on the smaller attemptRecord
// type. Keep the two in sync: any change to the eval ranking logic must be
// reflected here.

package headless

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
)

// attemptRecord captures one best-of-N attempt for the result JSON.
type attemptRecord struct {
	Index         int           `json:"index"`
	Outcome       agent.Outcome `json:"outcome"`
	ExitCode      int           `json:"exit_code"`
	PatchNonEmpty bool          `json:"patch_nonempty"`
	Answer        *string       `json:"answer"`
	Cost          *float64      `json:"cost_usd"`
	Usage         llm.Usage     `json:"usage"`
}

// selectorVerdict captures one pairwise comparison result (double-blind: two
// calls with swapped labels, counted as one verdict).
type selectorVerdict struct {
	PickRound1  string `json:"pick_round_1"` // "A" or "B" — the LABEL picked in round 1
	PickRound2  string `json:"pick_round_2"` // "A" or "B" — the LABEL picked in round 2 (positions swapped)
	Consistent  bool   `json:"consistent"`   // both rounds agreed on the same UNDERLYING patch
	WinnerIndex int    `json:"winner_index"` // index into attempts (0-based)
	Reason      string `json:"reason"`       // round 1's stated reason
}

// bestOfReport is the "best_of" object added to the result JSON when -best-of >= 2.
type bestOfReport struct {
	N             int               `json:"n"`
	Attempts      []attemptRecord   `json:"attempts"`
	Verdicts      []selectorVerdict `json:"verdicts"`
	SelectorModel string            `json:"selector_model"`
	Usage         llm.Usage         `json:"usage"`
	Cost          *float64          `json:"cost_usd"`
}

// bestOfSelector is the interface for comparative double-blind selection.
// Implementations call an LLM; tests inject a deterministic stub.
type bestOfSelector interface {
	// Compare asks the selector which of two patches better solves the task.
	// Returns "A" or "B", a short reason, token usage, and any error.
	Compare(ctx context.Context, task, patchA, patchB string) (pick string, reason string, usage llm.Usage, err error)
}

// sumUsage adds two Usage values field-wise.
func sumUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		CachedTokens:     a.CachedTokens + b.CachedTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		Cost:             a.Cost + b.Cost,
	}
}

// attemptStructuralRank mirrors eval.structuralRank (eval/bestof.go) on the
// smaller attemptRecord type. Keep in sync: any change there must be reflected
// here.
func attemptStructuralRank(a attemptRecord) int {
	if !a.PatchNonEmpty {
		return 3
	}
	switch a.Outcome {
	case agent.Answered:
		return 0
	case agent.HitCap:
		return 1
	default:
		return 2
	}
}

// selectBestByStructure mirrors eval.SelectBest over attemptRecord. Returns
// the index of the structurally-best attempt, breaking rank ties by earliest
// index. Returns -1 when empty.
func selectBestByStructure(attempts []attemptRecord) int {
	best := -1
	for i, a := range attempts {
		if best == -1 {
			best = i
			continue
		}
		b := attempts[best]
		if r, br := attemptStructuralRank(a), attemptStructuralRank(b); r < br || (r == br && a.Index < b.Index) {
			best = i
		}
	}
	return best
}

// runComparison executes ONE double-blind pairwise comparison between two
// attempts: round 1 (A=patchA, B=patchB), round 2 (A=patchB, B=patchA —
// labels swapped). Returns the verdict and accumulated usage.
func runComparison(ctx context.Context, task string, idxA, idxB int, patchA, patchB string, sel bestOfSelector) (selectorVerdict, llm.Usage) {
	vd := selectorVerdict{}
	var totalUsage llm.Usage

	// Round 1: A = patchA, B = patchB.
	pick1, reason1, u1, err1 := sel.Compare(ctx, task, patchA, patchB)
	totalUsage = sumUsage(totalUsage, u1)
	vd.PickRound1 = strings.ToUpper(strings.TrimSpace(pick1))
	if err1 != nil {
		vd.Consistent = false
		vd.Reason = fmt.Sprintf("selector error (round 1): %v", err1)
		return vd, totalUsage
	}

	// Round 2: A = patchB, B = patchA (swapped).
	pick2, _, u2, err2 := sel.Compare(ctx, task, patchB, patchA)
	totalUsage = sumUsage(totalUsage, u2)
	vd.PickRound2 = strings.ToUpper(strings.TrimSpace(pick2))
	if err2 != nil {
		vd.Consistent = false
		vd.Reason = fmt.Sprintf("selector error (round 2): %v", err2)
		return vd, totalUsage
	}

	vd.Reason = reason1

	// Determine if consistent: both rounds must agree on the same UNDERLYING patch.
	// Round 1: pick1 is "A" or "B" where A=patchA, B=patchB.
	// Round 2: pick2 is "A" or "B" where A=patchB, B=patchA (swapped).
	// If they picked the SAME label in both rounds, since labels were swapped
	// they actually picked DIFFERENT underlying patches → inconsistent.
	// If they picked DIFFERENT labels, they picked the SAME underlying patch → consistent.
	if vd.PickRound1 == vd.PickRound2 {
		vd.Consistent = false
	} else {
		vd.Consistent = true
	}

	if vd.Consistent {
		if vd.PickRound1 == "A" {
			vd.WinnerIndex = idxA
		} else {
			vd.WinnerIndex = idxB
		}
	}

	return vd, totalUsage
}

// selectBestOf runs the comparative double-blind tournament over attempts that
// produced a non-empty patch. patches[i] is the patch for attempts[i]
// (0-based), empty string if no patch.
//
// Returns the winner index (0-based) and the full selection report.
func selectBestOf(ctx context.Context, attempts []attemptRecord, patches []string, task string, sel bestOfSelector, selectorModel string) (winnerIndex int, report bestOfReport) {
	report.N = len(attempts)
	report.Attempts = attempts
	report.SelectorModel = selectorModel

	// Find candidates with non-empty patches.
	var candidates []int // indices into attempts (0-based)
	for i, a := range attempts {
		if a.PatchNonEmpty {
			candidates = append(candidates, i)
		}
	}

	// 0 non-empty patches: degrade to last attempt.
	if len(candidates) == 0 {
		winnerIndex = len(attempts) - 1
		return
	}

	// 1 non-empty patch: winner by default.
	if len(candidates) == 1 {
		winnerIndex = candidates[0]
		return
	}

	// >=2: pairwise tournament. Winner starts as first candidate, then each
	// subsequent candidate challenges.
	winner := candidates[0]
	for _, challenger := range candidates[1:] {
		vd, u := runComparison(ctx, task, winner, challenger, patches[winner], patches[challenger], sel)
		report.Usage = sumUsage(report.Usage, u)

		if vd.Consistent {
			winner = vd.WinnerIndex
		} else {
			// Fallback: structural rank between the two.
			best := selectBestByStructure([]attemptRecord{attempts[winner], attempts[challenger]})
			if best == 0 {
				// winner stays
			} else {
				winner = challenger
			}
			vd.WinnerIndex = winner
		}
		report.Verdicts = append(report.Verdicts, vd)
	}

	winnerIndex = winner
	return
}

// validateBestOfSetup rejects invalid best-of configuration before any selector
// or solver call can spend tokens.
func validateBestOfSetup(n int, selectModel, reviewModel string, inGitWorkspace bool) error {
	if n < 1 {
		return fmt.Errorf("best-of: -best-of must be >= 1")
	}
	if n < 2 {
		return nil
	}
	if strings.TrimSpace(selectModel) == "" && strings.TrimSpace(reviewModel) == "" {
		return fmt.Errorf("best-of: -select-model is required when -best-of >= 2 (or set -review-model to reuse it)")
	}
	if !inGitWorkspace {
		return fmt.Errorf("best-of: -best-of >= 2 requires cwd to be inside a git work tree, like -worktree")
	}
	return nil
}

// llmBestOfSelector is the production selector: one non-tool LLM call per round.
type llmBestOfSelector struct {
	Provider llm.Provider
	Model    string
	Effort   string
}

type selectorJSON struct {
	Pick   string `json:"pick"`
	Reason string `json:"reason"`
}

const bestOfSelectorSystem = `You are an independent patch selector. You will receive a TASK and two anonymous candidate git patches labeled A and B. Pick the patch that is more likely to correctly and completely accomplish the task without regressions. Judge only the patches; do not speculate about who wrote them.

Return ONLY JSON in this shape: {"pick":"A","reason":"short reason"} or {"pick":"B","reason":"short reason"}. No markdown.`

func (s llmBestOfSelector) Compare(ctx context.Context, task, patchA, patchB string) (string, string, llm.Usage, error) {
	if s.Provider == nil {
		return "", "", llm.Usage{}, fmt.Errorf("selector provider is nil")
	}
	prompt := fmt.Sprintf("TASK:\n%s\n\nPATCH A:\n%s\n\nPATCH B:\n%s\n", task, patchA, patchB)
	resp, err := s.Provider.Generate(ctx, llm.Request{
		Model:    s.Model,
		System:   bestOfSelectorSystem,
		Messages: []llm.Message{llm.User(prompt)},
		// reasoning models spend output budget on thinking before the JSON
		// verdict; 1024 starved gpt-5.5 to an empty Text() (bestof-validation 2026-07-03).
		MaxTokens:       16384,
		ReasoningEffort: s.Effort,
	})
	if err != nil {
		return "", "", llm.Usage{}, err
	}
	var out selectorJSON
	text := strings.TrimSpace(resp.Text())
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		// Be tolerant of tiny wrapper prose, but still require an unambiguous label.
		upper := strings.ToUpper(text)
		switch {
		case strings.Contains(upper, `"PICK":"A"`) || strings.HasPrefix(upper, "A"):
			out.Pick = "A"
		case strings.Contains(upper, `"PICK":"B"`) || strings.HasPrefix(upper, "B"):
			out.Pick = "B"
		default:
			return "", "", resp.Usage, fmt.Errorf("selector returned unparsable verdict: %q", text)
		}
		out.Reason = text
	}
	pick := strings.ToUpper(strings.TrimSpace(out.Pick))
	if pick != "A" && pick != "B" {
		return "", "", resp.Usage, fmt.Errorf("selector returned invalid pick %q", out.Pick)
	}
	return pick, strings.TrimSpace(out.Reason), resp.Usage, nil
}

func priceBestOfReport(report *bestOfReport, price PriceLookup) {
	if report == nil || strings.TrimSpace(report.SelectorModel) == "" {
		return
	}
	if report.Usage.Cost > 0 {
		c := report.Usage.Cost
		report.Cost = &c
		return
	}
	if price != nil {
		if cost, ok := price(report.SelectorModel, report.Usage); ok {
			report.Cost = &cost
		}
	}
}
