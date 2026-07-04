package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/AccursedGalaxy/mneme"
)

type finishInput struct {
	answer               string
	lastRunFailed        bool
	canContinue          bool
	trusted              bool
	verifyContinuePhrase string
	grounded             bool
	memoryScope          mneme.Scope
	unverifiedNotePrefix string
	reviewBlockedPrefix  string
}

type finishDecisionKind int

const (
	finishContinue finishDecisionKind = iota
	finishStop
	finishAnswered
)

type finishDecision struct {
	kind     finishDecisionKind
	feedback string
	outcome  Outcome
	reason   string
	answer   string
	memDone  <-chan struct{}
}

func continueWith(feedback string) finishDecision {
	return finishDecision{kind: finishContinue, feedback: feedback}
}

func stopWith(outcome Outcome, reason, answer string) finishDecision {
	return finishDecision{kind: finishStop, outcome: outcome, reason: reason, answer: answer}
}

func answeredWith(answer string, memDone <-chan struct{}) finishDecision {
	return finishDecision{kind: finishAnswered, answer: answer, memDone: memDone}
}

func (g *gates) finish(ctx context.Context, in finishInput) finishDecision {
	outcome, reason, noContinue := g.verifyFinish(ctx, in.lastRunFailed, in.trusted)
	if outcome == ScopeViolation {
		// Answer stays empty on a scope violation (and on cancel below) — the
		// pre-extraction sites set only Outcome+Reason on these two paths, and
		// RunNative's N1 salvage defer still fills Answer from the model's last
		// prose for relaying callers.
		return stopWith(ScopeViolation, reason, "")
	}
	// A caller cancel mid-finish stops the run cleanly as Canceled. We check
	// ctx.Err() because signal.NotifyContext (Go 1.26+) cancels with a custom
	// signalError cause, and Err() is cause-agnostic while still distinguishing
	// deadline expiry.
	if errors.Is(ctx.Err(), context.Canceled) {
		return stopWith(Canceled, "run canceled by the caller (interrupt)", "")
	}
	if reason != "" && g.cfg.VerifyContinue && in.canContinue && !noContinue {
		g.cfg.Obs.Note("finish rejected (not verified) — continuing")
		return continueWith("OBSERVATION:\nNot finished — " + in.verifyContinuePhrase + ", but the task is not verified:\n" + reason + "\nKeep working: fix the code and re-run until it passes.")
	}
	if reason != "" {
		prefix := in.unverifiedNotePrefix
		if prefix == "" {
			prefix = "answer not verified"
		}
		g.cfg.Obs.Note(prefix + " — " + reason)
		return stopWith(outcome, reason, in.answer)
	}
	if strings.TrimSpace(in.answer) == "" {
		g.cfg.Obs.Note("empty final answer — recording as unverified, not a clean pass")
		return stopWith(Unverified, "empty final answer — the model stopped without producing an answer", in.answer)
	}
	if !in.trusted {
		if fb, blockReason := g.reviewFinish(ctx, in.canContinue); fb != "" {
			g.cfg.Obs.Note("finish rejected (review blockers) — continuing")
			return continueWith("OBSERVATION:\n" + fb)
		} else if blockReason != "" {
			prefix := in.reviewBlockedPrefix
			if prefix == "" {
				prefix = "answer blocked by review"
			}
			g.cfg.Obs.Note(prefix + " — " + blockReason)
			return stopWith(Unverified, blockReason, in.answer)
		}
	}
	g.cfg.Obs.Done(in.answer)
	if in.grounded {
		return answeredWith(in.answer, rememberAsync(ctx, g.cfg.Obs, g.cfg.Memory, in.memoryScope, g.cfg.Task, in.answer))
	}
	if g.cfg.Memory != nil {
		g.cfg.Obs.Note("memory: answer not tool-verified this run — not stored (avoids amplifying guessed/recalled facts)")
	}
	return answeredWith(in.answer, nil)
}
