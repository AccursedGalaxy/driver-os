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

	// SessionKey and RoundOffset are optional continuity hooks for callers that
	// stitch several one-pass reviews into one logical review gate. They keep the
	// reviewer continuation handle stable and make ReviewInput.Round match the
	// logical pass number while preserving the original BaseTree.
	SessionKey  string
	RoundOffset int
}

// ReviewExistingWorkspace runs the existing closing review gate once against an
// already-mutated workspace. It exposes the gate without exposing the internal
// gates/reviewState types, so callers that did not start a normal agent loop can
// still reuse reviewer classification, repro execution, fates, usage reporting,
// and fail-open behavior.
func ReviewExistingWorkspace(ctx context.Context, cfg Config, runTimeout time.Duration, opts ReviewPassOptions) (feedback string, blockReason string, report *ReviewReport) {
	return reviewExistingWorkspacePass(ctx, cfg, runTimeout, opts, true)
}

func reviewExistingWorkspacePass(ctx context.Context, cfg Config, runTimeout time.Duration, opts ReviewPassOptions, canContinue bool) (feedback string, blockReason string, report *ReviewReport) {
	if cfg.Obs == nil {
		cfg.Obs = nopObserver{}
	}
	// The review gate judges a finished tree — there is no "before any
	// changes" to baseline, so skip the pre-flight VerifyCmd execution
	// (same pattern as NewGate).
	cfg.SkipVerifyBaseline = true
	gs, err := newGates(ctx, cfg, runTimeout)
	if err != nil {
		return "", err.Error(), nil
	}
	if gs.review != nil {
		if opts.BaseTree != "" {
			gs.review.baseTree = opts.BaseTree
			gs.review.skip = ""
		}
		if opts.SessionKey != "" {
			gs.review.sessionKey = opts.SessionKey
		}
		if opts.RoundOffset > 0 {
			gs.review.rounds = opts.RoundOffset
		}
	}
	feedback, blockReason = gs.reviewFinish(ctx, canContinue)
	return feedback, blockReason, gs.reviewReport()
}

// ReviewAndRepairExistingWorkspace applies the review gate to an already chosen
// result/workspace and, when review produces repair feedback, gives the normal
// agent loop continuation runs in the same workspace. Every review pass is
// against the original pre-solve base tree; repair runs have their reviewer
// disabled so they cannot silently re-baseline the gate to the already-patched
// workspace. A blocking, unrepaired review marks the chosen result Unverified;
// callers must not fall back to a lower-ranked candidate.
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

	maxRounds := cfg.ReviewRounds
	if maxRounds <= 0 {
		maxRounds = DefaultReviewRounds
	}
	sessionKey := opts.SessionKey
	if sessionKey == "" {
		sessionKey = newRunID()
	}

	current := &out
	accumSolveUsage := out.Usage
	var combined *ReviewReport
	for pass := 1; pass <= maxRounds; pass++ {
		passOpts := opts
		passOpts.SessionKey = sessionKey
		passOpts.RoundOffset = pass - 1
		feedback, blockReason, report := reviewExistingWorkspacePass(ctx, cfg, cfg.RunTimeout, passOpts, pass < maxRounds)
		mergeReviewReports(&combined, report)
		current.Review = combined

		if blockReason != "" {
			current.Outcome = Unverified
			current.Reason = blockReason
			return current, nil
		}
		if feedback == "" {
			if reason := combinedReviewRequiredBlockReason(cfg, combined); reason != "" {
				current.Outcome = Unverified
				current.Reason = reason
			}
			return current, nil
		}
		if loop == nil {
			current.Outcome = Unverified
			current.Reason = "review blockers remain, but no repair loop is configured"
			return current, nil
		}

		repairCfg := cfg
		repairCfg.Reviewer = nil
		repairCfg.Task = fmt.Sprintf("%s\n\nThe selected patch was blocked by the review gate. Address this review feedback, then finish again:\n\n%s", cfg.Task, feedback)
		res, err := loop(ctx, repairCfg)
		if res == nil {
			return current, err
		}
		// Fold solve tokens from the selected winner and any earlier repairs into
		// the latest repair result so total cost reflects the whole solve path.
		res.Usage = addUsage(res.Usage, accumSolveUsage)
		accumSolveUsage = res.Usage
		res.Review = combined
		current = res
		if err != nil {
			return current, err
		}
		// Matching the in-loop gate, reaching another review pass means the blockers
		// that were fed back from the previous pass are no longer pending expiry.
		markExpiredBlockersRepaired(combined)
	}
	return current, nil
}

func combinedReviewRequiredBlockReason(cfg Config, report *ReviewReport) string {
	if !cfg.ReviewRequired || report == nil || !isReviewInfrastructureStatus(report.Status) {
		return ""
	}
	return fmt.Sprintf("review required but review status is %s", report.Status)
}

func mergeReviewReports(dst **ReviewReport, src *ReviewReport) {
	if src == nil {
		return
	}
	if *dst == nil {
		cp := *src
		cp.ReviewerRuns = append([]string(nil), src.ReviewerRuns...)
		cp.Summaries = append([]string(nil), src.Summaries...)
		cp.Findings = append([]ReviewedFinding(nil), src.Findings...)
		*dst = &cp
		recountReviewBlockers(*dst)
		return
	}
	d := *dst
	if src.Rounds > d.Rounds {
		d.Rounds = src.Rounds
	}
	if !isReviewInfrastructureStatus(d.Status) || d.Status == "" {
		d.Status = src.Status
	}
	d.Blocked = src.Blocked
	if d.Skipped == "" {
		d.Skipped = src.Skipped
	}
	if d.ReviewerModel == "" {
		d.ReviewerModel = src.ReviewerModel
	}
	d.ReviewerRuns = append(d.ReviewerRuns, src.ReviewerRuns...)
	d.Summaries = append(d.Summaries, src.Summaries...)
	d.Findings = append(d.Findings, src.Findings...)
	d.Usage = addUsage(d.Usage, src.Usage)
	recountReviewBlockers(d)
}

func markExpiredBlockersRepaired(report *ReviewReport) {
	if report == nil {
		return
	}
	for i := range report.Findings {
		if report.Findings[i].Fate == FateExpired && report.Findings[i].Severity == "blocker" {
			report.Findings[i].Fate = FateRepaired
		}
	}
}

func recountReviewBlockers(report *ReviewReport) {
	if report == nil {
		return
	}
	var confirmed, unconfirmed int
	for _, f := range report.Findings {
		if f.Severity != "blocker" {
			continue
		}
		if f.Confirmed {
			confirmed++
		} else {
			unconfirmed++
		}
	}
	report.ConfirmedBlockers = confirmed
	report.UnconfirmedBlockers = unconfirmed
}
