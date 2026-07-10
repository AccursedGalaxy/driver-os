package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// diagnoseSource runs cfg.DiagnoseCmd as slice 1's diagnostics SOURCE (a fast
// compile/type check — go build/go vet via the sandbox) and reports its output and
// a three-way diagState. The source/surfacing split (see docs/specs/CODE-INTELLIGENCE.md)
// keeps this single call as the seam a future persistent-gopls client slots
// behind, leaving the loops' surfacing logic untouched. An infra failure to even
// start the check (or a "" command) is diagUnknown, NOT clean: we never fabricate
// a build error out of an infrastructure fault (P6) — and just as importantly we
// never fabricate a GREEN out of one, because the callers reset the stuck counter
// on clean and a transient exec fault on a genuinely-red build would silently
// disarm the feed.
// verifySandbox is the sandbox the closing verification/diagnostics commands run
// on: VerifySandbox when set, else Sandbox. The split lets -session route the
// model's `run` tool through a stateful session while these trust-critical gates
// run in a clean context (see Config.VerifySandbox / ../SESSION.md).
func (c Config) verifySandbox() sandbox.Sandbox {
	if c.VerifySandbox != nil {
		return c.VerifySandbox
	}
	return c.Sandbox
}

// diagState is diagnoseSource's verdict: green, red, or no-signal. The zero
// value is diagUnknown on purpose — "we couldn't check" is the safe default.
type diagState int

const (
	diagUnknown diagState = iota // the check couldn't run — no signal either way.
	diagClean                    // the check ran and exited 0 — genuinely green.
	diagDirty                    // the check ran and failed — report carries the errors.
)

func diagnoseSource(ctx context.Context, cfg Config, timeout time.Duration) (report string, state diagState) {
	if cfg.DiagnoseCmd == "" {
		return "", diagUnknown
	}
	out, err := runOp(ctx, cfg.verifySandbox(), cfg.DiagnoseCmd, timeout)
	if err != nil {
		return "", diagUnknown
	}
	if isRunFailure(out) {
		return out, diagDirty
	}
	return "", diagClean
}

// diagnosticsMessage renders the stuck-with-a-broken-build feed: the failing command
// and its (already clipped by formatRun) output, framed explicitly as INFORMATION, not
// a stop. Shared by both loops so the wording is identical regardless of protocol.
func diagnosticsMessage(cmd, report string) string {
	return fmt.Sprintf("[diagnostics] your recent edits do not build yet — `%s` reports:\n%s\n"+
		"Fix these errors and continue. This is informational, not a stop.", cmd, report)
}

// verifyTimeout resolves the bound for closing VerifyCmd executions. When
// Config.VerifyTimeout is explicitly set it wins; otherwise the bound is the
// LARGER of the resolved per-`run`-tool RunTimeout and 5 minutes — a verify
// suite is routinely slower than a single interactive command, and a too-short
// bound turns "couldn't finish checking" into a false "did not pass".
func verifyTimeout(cfg Config, runTimeout time.Duration) time.Duration {
	if cfg.VerifyTimeout > 0 {
		return cfg.VerifyTimeout
	}
	const floor = 5 * time.Minute
	if runTimeout > floor {
		return runTimeout
	}
	return floor
}

// verifyRun executes cfg.VerifyCmd under the ONE context policy every closing
// verification gate shares — answer, FinishTool, kill, cap, and deadline paths
// alike (review #3; the paths used to diverge, so a cancel at the instant the
// model answered recorded Unverified while the same run killed a turn later got
// upgraded to Answered):
//
//   - A USER cancel skips the check entirely (skipped=true) — the user asked us
//     to stop, so we spend nothing more; the caller reports the unverified /
//     un-upgraded outcome honestly.
//   - Anything else (including a wall-clock or caller deadline expiry) runs the
//     check DETACHED from cancellation (context.WithoutCancel) — an expired
//     budget must not block the final ground-truth check — bounded by the
//     sandbox's own command timeout so it can't hang.
func verifyRun(ctx context.Context, cfg Config, runTimeout time.Duration) (out string, skipped bool, err error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", true, nil
	}
	out, err = runOp(context.WithoutCancel(ctx), cfg.verifySandbox(), cfg.VerifyCmd, verifyTimeout(cfg, runTimeout))
	if cfg.evidence != nil && cfg.evidence.closingReady {
		status := EvidencePassed
		switch {
		case err != nil:
			status = EvidenceInconclusive
		case isRunTimeout(out) || infraFaultSignature(out) != "":
			status = EvidenceInconclusive
		case isRunFailure(out):
			status = EvidenceFailed
		}
		cfg.evidence.recordVerify(cfg.VerifyCmd, status)
	}
	return out, false, err
}

func infraFaultSignature(out string) string {
	if strings.Contains(out, "--- FAIL") {
		return ""
	}
	lower := strings.ToLower(out)
	for _, sig := range []string{
		"disk quota exceeded",
		"no space left on device",
		"cannot allocate memory",
		"too many open files",
	} {
		if strings.Contains(lower, sig) {
			return sig
		}
	}
	return ""
}

func isInfraFault(out string) bool { return infraFaultSignature(out) != "" }

// verifyTermination decides whether a model's done-signal actually holds (P5/HP-5)
// and returns a non-empty reason when it does NOT (so the caller records Unverified
// instead of Answered). Two checks, in precedence order:
//
//   - VerifyCmd (authoritative): the caller named a success command, so re-run it.
//     A non-zero exit is ground truth — independent of what the model claimed, and
//     independent of any model cooperation. This is the precise gate that turns the
//     DOGFOOD R9/R10 false positives (a model that stops while the build is red)
//     into honest non-passes.
//   - VerifyLastRun (heuristic fallback, opt-in): with no VerifyCmd, a silent finish
//     is suspect when the most recent `run` was still failing. Weaker — a legitimate
//     absence answer can follow a non-zero exit (grep-no-match) — hence opt-in.
//
// With neither configured it returns "" (the historical behavior: trust the answer).
func verifyTermination(ctx context.Context, cfg Config, lastRunFailed bool, runTimeout time.Duration) (reason, verifyOut string) {
	if cfg.VerifyCmd != "" {
		out, skipped, err := verifyRun(ctx, cfg, runTimeout)
		if skipped { // user cancel — no check ran, so the claim stays unconfirmed (and no VerifyResult: nothing was measured).
			return fmt.Sprintf("run canceled before verification command %q could confirm success", cfg.VerifyCmd), ""
		}
		notifyVerify(cfg.Obs, cfg.VerifyCmd, err == nil && !isRunFailure(out))
		if err != nil { // couldn't even start it — we cannot confirm success.
			return fmt.Sprintf("could not run verification command %q: %v", cfg.VerifyCmd, err), ""
		}
		if isRunFailure(out) && !isRunTimeout(out) && isInfraFault(out) {
			// A transient environment fault at the FINAL gate would cost the
			// whole run's outcome — retry once before conceding. (The
			// 2026-07-07 incident: tmpfs EDQUOT failed the closing verify;
			// the identical command re-ran green.)
			retry, retrySkipped, retryErr := verifyRun(ctx, cfg, runTimeout)
			if !retrySkipped && retryErr == nil {
				notifyVerify(cfg.Obs, cfg.VerifyCmd, !isRunFailure(retry))
				out = retry
			}
		}
		if isRunFailure(out) {
			if isRunTimeout(out) {
				bound := verifyTimeout(cfg, runTimeout)
				return fmt.Sprintf("verification command %q was INCONCLUSIVE (timed out after %s — raise -verify-timeout):\n%s", cfg.VerifyCmd, bound, out), out
			}
			if sig := infraFaultSignature(out); sig != "" {
				return fmt.Sprintf("verification command %q was INCONCLUSIVE (environment fault: %s — disk quota / no space / OOM, not a code failure; retried once):\n%s", cfg.VerifyCmd, sig, out), out
			}
			return fmt.Sprintf("verification command %q did not pass:\n%s", cfg.VerifyCmd, out), out
		}
		return "", out
	}
	if cfg.VerifyLastRun && lastRunFailed {
		return "the most recent command run was still failing and nothing succeeded after it — the task does not look complete", ""
	}
	return "", ""
}
