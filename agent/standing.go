package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AccursedGalaxy/driver-os/vcs"
)

const (
	standingDiffCap     = 6000
	standingStatFileCap = 40
	standingGateTailCap = 800
)

var standingDiffTrees = vcs.DiffTrees

type standingState struct {
	indexPath string
	lastTree  string
	lastDiff  string
}

func newStandingState() *standingState {
	dir, err := os.MkdirTemp("", "driver-os-standing-index-")
	if err != nil {
		return &standingState{}
	}
	return &standingState{indexPath: filepath.Join(dir, "index")}
}

func (s *standingState) cleanup() {
	if s != nil && s.indexPath != "" {
		_ = os.RemoveAll(filepath.Dir(s.indexPath))
	}
}

func (s *standingState) currentTree(ctx context.Context, cfg Config) (string, error) {
	if s == nil || cfg.Root == "" {
		return "", fmt.Errorf("no workspace root")
	}
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	defer cancel()
	return vcs.WriteTreeCached(gctx, cfg.Root, s.indexPath)
}

func (s *standingState) block(ctx context.Context, cfg Config, gs *gates, tr *turnTracker, turn int) string {
	if s == nil || !cfg.StandingContext {
		return ""
	}
	cur, curErr := s.currentTree(ctx, cfg)
	base := ""
	baselineMeasured, baselineRed := false, false
	if gs != nil {
		base = gs.standingBaseTree()
		baselineMeasured = gs.verifyBaselineMeasured
		baselineRed = gs.verifyBaselineRed
	}
	unchanged := curErr == nil && base != "" && cur == base
	diffSection := "# Your changes so far: unavailable (could not read working tree)"
	if curErr == nil {
		s.renderDiff(ctx, cfg, base, cur)
		diffSection = s.lastDiff
	}
	return renderStandingBlock(diffSection, tr, strings.TrimSpace(cfg.VerifyCmd), cur, curErr != nil, unchanged, baselineMeasured, baselineRed)
}

func (s *standingState) renderDiff(ctx context.Context, cfg Config, base, cur string) {
	if base == "" {
		s.lastTree, s.lastDiff = cur, "# Your changes so far: unavailable (no session-start git baseline)"
		return
	}
	if cur == s.lastTree && s.lastDiff != "" {
		return
	}
	s.lastTree = cur
	if cur == base {
		s.lastDiff = "# Your changes so far: none yet (working tree matches session start)"
		return
	}
	gctx, cancel := gateContext(ctx, gateDiffTimeout)
	stat, statErr := vcs.DiffTreesStat(gctx, cfg.Root, base, cur)
	cancel()
	if statErr != nil {
		s.lastDiff = "# Your changes so far: unavailable (could not render diff stat: " + statErr.Error() + ")"
		return
	}
	gctx, cancel = gateContext(ctx, gateDiffTimeout)
	diff, diffErr := standingDiffTrees(gctx, cfg.Root, base, cur)
	cancel()
	if diffErr != nil {
		s.lastDiff = "# Your changes so far: unavailable (could not render diff: " + diffErr.Error() + ")"
		return
	}
	s.lastDiff = renderDiffSection(stat, diff)
}

func renderDiffSection(stat, diff string) string {
	stat = strings.TrimRight(stat, "\n")
	diff = strings.TrimRight(diff, "\n")
	body := boundedStat(stat)
	if len([]rune(body)) >= standingDiffCap || diff == "" {
		return "# Your changes so far (vs session start)\n" + clip(body, standingDiffCap)
	}
	if len([]rune(diff)) <= standingDiffCap-len([]rune(body))-2 {
		if body != "" {
			body += "\n" + diff
		} else {
			body = diff
		}
		return "# Your changes so far (vs session start)\n" + clip(body, standingDiffCap)
	}
	notice := fmt.Sprintf("(full diff is %d runes — over the %d cap; showing summary only, use read_file for specifics)", len([]rune(diff)), standingDiffCap)
	if body != "" {
		body += "\n" + notice
	} else {
		body = notice
	}
	return "# Your changes so far (vs session start)\n" + clip(body, standingDiffCap)
}

func boundedStat(stat string) string {
	if len([]rune(stat)) <= standingDiffCap {
		return stat
	}
	lines := strings.Split(strings.TrimRight(stat, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	short := strings.TrimSpace(lines[len(lines)-1])
	fileLines := lines[:len(lines)-1]
	keep := standingStatFileCap
	if keep > len(fileLines) {
		keep = len(fileLines)
	}
	out := make([]string, 0, keep+3)
	if short != "" {
		out = append(out, short)
	}
	out = append(out, fileLines[:keep]...)
	if more := len(fileLines) - keep; more > 0 {
		out = append(out, fmt.Sprintf("… and %d more files", more))
	}
	return strings.Join(out, "\n")
}

func renderStandingBlock(diffSection string, tr *turnTracker, verifyCmd, curTree string, curUnknown bool, unchanged bool, baselineMeasured, baselineRed bool) string {
	var b strings.Builder
	b.WriteString("=== STANDING CONTEXT (auto-refreshed each turn — do NOT run `git diff` or re-read\n")
	b.WriteString("    files just to reconstruct this summary; read files when you need details not\n")
	b.WriteString("    shown here) ===\n\n")
	b.WriteString(strings.TrimRight(diffSection, "\n"))
	b.WriteString("\n\n# Last verification\n")
	if baselineMeasured {
		status := "GREEN"
		if baselineRed {
			status = "RED"
		}
		fmt.Fprintf(&b, "baseline at session start: %s\n", status)
	}
	b.WriteString(renderVerification(tr, verifyCmd, curTree, curUnknown, unchanged))
	b.WriteString("\n=== END STANDING CONTEXT ===")
	return b.String()
}

func renderVerification(tr *turnTracker, verifyCmd, curTree string, curUnknown bool, unchanged bool) string {
	if tr == nil {
		tr = &turnTracker{}
	}
	var lines []string
	if verifyCmd != "" {
		if tr.lastVerify.Command == "" {
			if unchanged {
				lines = append(lines, fmt.Sprintf("gate: `%s` — no file changes yet this session; nothing to verify (the harness runs it authoritatively at finish)", verifyCmd))
			} else {
				hint := ""
				if verifySkipFilter(verifyCmd) != "" {
					hint = " The gate deliberately skips these — carry the filter or you will see unrelated red tests."
				}
				lines = append(lines, fmt.Sprintf("gate: `%s` — NOT run yet this session; the closing gate runs authoritatively at finish. For mid-run confidence, run only tests scoped to the package(s) you changed.%s", verifyCmd, hint))
			}
		} else {
			tag := freshnessTag(tr.lastVerify.Tree, curTree, curUnknown)
			if unchanged {
				tag = ""
			}
			lines = append(lines, fmt.Sprintf("gate: `%s` → %s%s", tr.lastVerify.Command, verificationWord(tr.lastVerify), tag))
			if tr.lastVerify.Output != "" {
				lines = append(lines, tr.lastVerify.Output)
			}
		}
		if tr.lastRun.Command != "" && strings.TrimSpace(tr.lastRun.Command) != strings.TrimSpace(tr.lastVerify.Command) {
			lines = append(lines, fmt.Sprintf("last check (not the gate): `%s` → %s%s", tr.lastRun.Command, verificationWord(tr.lastRun), freshnessTag(tr.lastRun.Tree, curTree, curUnknown)))
		}
		return strings.Join(lines, "\n")
	}
	if tr.lastRun.Command != "" {
		lines = append(lines, fmt.Sprintf("last check: `%s` → %s%s", tr.lastRun.Command, verificationWord(tr.lastRun), freshnessTag(tr.lastRun.Tree, curTree, curUnknown)))
		if tr.lastRun.Output != "" {
			lines = append(lines, tr.lastRun.Output)
		}
		return strings.Join(lines, "\n")
	}
	return "last check: none yet"
}

func verificationWord(record VerificationRecord) string {
	switch record.Status {
	case EvidencePassed:
		return "PASS"
	case EvidenceFailed:
		return "FAIL"
	}
	switch record.Cause {
	case VerificationCauseTimeout:
		return "TIMEOUT"
	case VerificationCauseEnvironment:
		return "ENV-FAULT"
	case VerificationCauseCancellation:
		return "CANCELED"
	case VerificationCauseExecutionError:
		return "EXEC-ERROR"
	default:
		return "UNKNOWN"
	}
}

func freshnessTag(runTree, curTree string, unknown bool) string {
	if runTree == "" {
		return ""
	}
	if unknown || curTree == "" {
		return "  [FRESHNESS UNKNOWN: could not read working tree]"
	}
	if runTree != curTree {
		return "  [STALE: files changed since this ran — re-run to confirm]"
	}
	return ""
}

func tailClip(s string, capRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= capRunes {
		return string(r)
	}
	return fmt.Sprintf("...[%d runes elided]...\n%s", len(r)-capRunes, string(r[len(r)-capRunes:]))
}
