package headless

import (
	"github.com/AccursedGalaxy/driver-os/cli"
)

const usageHeader = `driver-agent — headless think→act→observe coding agent.

Usage: driver-agent -task '<goal>' [flags]
       cat issue.md | driver-agent -task - [flags]

The task runs to completion unattended; stdout carries the result (-format
json|ndjson for machines), stderr carries the live trace. Exit codes carry the
outcome class (0 answered · 2 unverified · 3 resource cap · 4 stuck ·
5 provider · 6 refused · 7 canceled · 8 scope violation · 1 setup error).
Full contract: docs/specs/CLI-SCRIPTABLE.md.`

// usageGroups categorizes every cmd/agent flag for -help. A flag missing from
// every group falls into an "Other" section at render time, and
// TestUsageGroupsComplete fails — add new flags to a group when defining them.
var usageGroups = []cli.UsageGroup{
	{Title: "Task & model", Flags: []string{
		"task", "trust", "profile", "provider", "model", "effort", "protocol", "prompt-profile", "ladder",
	}},
	{Title: "Output & artifacts", Flags: []string{
		"format", "trace", "trace-file", "report", "transcript-dir", "ledger",
	}},
	{Title: "Verification gates", Flags: []string{
		"verify-cmd", "verify-baseline", "verify-timeout", "verify-last-run",
		"verify-continue", "require-diff", "test-fence", "diff-scope",
		"diagnose-cmd", "diagnose-after-edits", "repro-first", "repro-gate",
	}},
	{Title: "Review gate", Flags: []string{
		"review-model", "review-effort", "review-rounds", "review-required",
		"review-optional", "review-unverified",
	}},
	{Title: "Plan stage & best-of-N", Flags: []string{
		"plan-model", "plan-effort", "best-of", "select-model",
	}},
	{Title: "Gated repo mode", Flags: []string{
		"review", "approve", "review-action",
	}},
	{Title: "Isolation & sandbox", Flags: []string{
		"worktree", "keep-worktree", "sandbox", "runtime", "network", "untrusted", "session",
	}},
	{Title: "Limits & budget", Flags: []string{
		"max-iters", "max-tokens", "max-wall", "max-total-tokens", "budget", "allow-unpriced-spend", "run-timeout",
	}},
	{Title: "Context & steering", Flags: []string{
		"memory", "skills", "batch-reads", "boot-context", "read-window",
		"read-outline", "codeact", "churn-nudge", "finish-nudge",
	}},
}
