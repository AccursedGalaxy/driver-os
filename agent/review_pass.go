package agent

import (
	"context"
	"fmt"
	"time"
)

// ReviewPassOptions configures a closing review gate run over an existing
// workspace. BaseTree, when set, is the git tree to diff the current workspace
// against. Best-of-N uses this to review the selected attempt against its
// pre-solve HEAD rather than accidentally snapshotting the already-patched tree
// as the review baseline.
type ReviewPassOptions struct {
	BaseTree string
}

// ReviewExistingWorkspace runs the existing closing review gate once against an
// already-mutated workspace. It exposes the gate without exposing the internal
// gates/reviewState types, so callers that did not start a normal agent loop can
// still reuse reviewer classification, repro execution, fates, usage reporting,
// and fail-open behavior.
func ReviewExistingWorkspace(ctx context.Context, cfg Config, runTimeout time.Duration, opts ReviewPassOptions) (feedback string, blockReason string, report *ReviewReport) {
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}
	// The review gate judges a finished tree — there is no "before any
	// changes" to baseline, so skip the pre-flight VerifyCmd execution
	// (same pattern as NewGate).
	cfg.SkipVerifyBaseline = true
	gs := newGates(ctx, cfg, runTimeout)
	if gs.review != nil && opts.BaseTree != "" {
		gs.review.baseTree = opts.BaseTree
		gs.review.skip = ""
	}
	feedback, blockReason = gs.reviewFinish(ctx, true)
	return feedback, blockReason, gs.reviewReport()
}

// ReviewAndRepairExistingWorkspace applies the review gate to an already chosen
// result/workspace and, when the first review round produces repair feedback,
// gives the normal agent loop one continuation run in the same workspace. A
// blocking, unrepaired review marks the chosen result Unverified; callers must
// not fall back to a lower-ranked candidate.
func ReviewAndRepairExistingWorkspace(ctx context.Context, cfg Config, base *RunResult, opts ReviewPassOptions, loop LoopFunc) (*RunResult, error) {
	var out RunResult
	if base != nil {
		out = *base
	} else {
		out = RunResult{Task: cfg.Task, Root: cfg.Root, Outcome: Answered}
	}
	if cfg.Reviewer == nil {
		return &out, nil
	}

	feedback, blockReason, report := ReviewExistingWorkspace(ctx, cfg, cfg.RunTimeout, opts)
	out.Review = report
	if blockReason != "" {
		out.Outcome = Unverified
		out.Reason = blockReason
		return &out, nil
	}
	if feedback == "" {
		return &out, nil
	}
	if loop == nil {
		out.Outcome = Unverified
		out.Reason = "review blockers remain, but no repair loop is configured"
		return &out, nil
	}

	repairCfg := cfg
	repairCfg.Task = fmt.Sprintf("%s\n\nThe selected patch was blocked by the review gate. Address this review feedback, then finish again:\n\n%s", cfg.Task, feedback)
	res, err := loop(ctx, repairCfg)
	if res == nil {
		return &out, err
	}
	// Fold base's solve tokens into the repair result so total cost reflects
	// both the winner's original solve AND the repair turns.
	if base != nil {
		res.Usage.PromptTokens += base.Usage.PromptTokens
		res.Usage.CompletionTokens += base.Usage.CompletionTokens
		res.Usage.TotalTokens += base.Usage.TotalTokens
		res.Usage.CachedTokens += base.Usage.CachedTokens
		res.Usage.ReasoningTokens += base.Usage.ReasoningTokens
	}
	return res, err
}
