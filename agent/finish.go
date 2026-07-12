package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/AccursedGalaxy/driver-os/memory"
)

type finishInput struct {
	answer               string
	lastRunFailed        bool
	canContinue          bool
	trusted              bool
	verifyContinuePhrase string
	grounded             bool
	memoryScope          memory.Scope
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
	if reason != "" && g.d.vs.VerifyContinue && in.canContinue && !noContinue {
		g.d.rt.Obs.Note("finish rejected (not verified) — continuing")
		return continueWith("OBSERVATION:\nNot finished — " + in.verifyContinuePhrase + ", but the task is not verified:\n" + reason + "\nKeep working: fix the code and re-run until it passes.")
	}
	if reason != "" {
		prefix := in.unverifiedNotePrefix
		if prefix == "" {
			prefix = "answer not verified"
		}
		g.reviewUnverified(ctx)
		g.d.rt.Obs.Note(prefix + " — " + reason)
		return stopWith(outcome, reason, in.answer)
	}
	if fb, detail := g.reproFinish(ctx); fb != "" {
		// repro_missing is a protocol step (declare_repro or skip_repro), not
		// a red gate: feed it back whenever the loop can continue, even
		// without -verify-continue — otherwise the first answer attempt dies
		// before the model gets the nudge. repro_red mirrors verify-red.
		if in.canContinue && (detail == "repro_missing" || g.d.vs.VerifyContinue) {
			g.d.rt.Obs.Note("finish rejected (repro-first) — continuing")
			return continueWith("OBSERVATION:\nNot finished — " + fb + "\nKeep working: satisfy the repro-first gate before answering.")
		}
		g.reviewUnverified(ctx)
		g.d.rt.Obs.Note("answer not verified — " + detail)
		return stopWith(Unverified, detail, in.answer)
	}
	if strings.TrimSpace(in.answer) == "" {
		g.reviewUnverified(ctx)
		g.d.rt.Obs.Note("empty final answer — recording as unverified, not a clean pass")
		return stopWith(Unverified, "empty final answer — the model stopped without producing an answer", in.answer)
	}
	if !in.trusted {
		if fb, blockReason := g.reviewFinish(ctx, in.canContinue); fb != "" {
			g.d.rt.Obs.Note("finish rejected (review blockers) — continuing")
			return continueWith("OBSERVATION:\n" + fb)
		} else if blockReason != "" {
			prefix := in.reviewBlockedPrefix
			if prefix == "" {
				prefix = "answer blocked by review"
			}
			g.d.rt.Obs.Note(prefix + " — " + blockReason)
			return stopWith(Unverified, blockReason, in.answer)
		}
	}
	g.d.rt.Obs.Done(in.answer)
	if in.grounded {
		if g.d.pol.DisableMemoryStore && g.d.rt.Memory != nil {
			g.d.rt.Obs.Note("memory: store disabled for this run")
			return answeredWith(in.answer, nil)
		}
		return answeredWith(in.answer, rememberAsync(ctx, g.d.rt.Obs, g.d.rt.Memory, in.memoryScope, g.d.task, in.answer))
	}
	if g.d.rt.Memory != nil {
		g.d.rt.Obs.Note("memory: answer not tool-verified this run — not stored (avoids amplifying guessed/recalled facts)")
	}
	return answeredWith(in.answer, nil)
}
