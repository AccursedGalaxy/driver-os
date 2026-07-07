package gobench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const LeakScreenVersion = "gobench-leak-screen/0.1.0"
const DefaultLeakNgramSize = 8
const DefaultLeakThreshold = 0.05

func RunLeakScreen(ctx context.Context, inst Instance, checkoutDir string, threshold float64, screenedAt string) (LeakScreen, error) {
	raw, err := runGitCaptured(ctx, checkoutDir, "diff", "--unified=0", inst.BaseCommit+".."+inst.GoldCommit, "--", ".")
	if err != nil {
		return LeakScreen{}, err
	}
	diffText, excluded := screenedDiffText(raw)
	if threshold == 0 {
		threshold = DefaultLeakThreshold
	}
	score := NgramOverlapScore(inst.ProblemStatement, diffText, DefaultLeakNgramSize)
	return LeakScreen{Method: "ngram-overlap", NgramSize: DefaultLeakNgramSize, Tokenization: "word-lower-nonalnum", Passed: score <= threshold, Score: score, Threshold: threshold, DiffRange: inst.BaseCommit + ".." + inst.GoldCommit, ExcludedFiles: excluded, StatementHash: sha256Hex(inst.ProblemStatement), GoldDiffHash: sha256Hex(diffText), ToolVersion: LeakScreenVersion, ScreenedAt: screenedAt}, nil
}

var wordRe = regexp.MustCompile(`[A-Za-z0-9_]+`)

func NgramOverlapScore(statement, diff string, n int) float64 {
	if n <= 0 {
		return 0
	}
	st := tokenize(statement)
	df := tokenize(diff)
	total := len(st) - n + 1
	if total <= 0 {
		return 0
	}
	set := map[string]bool{}
	for i := 0; i+n <= len(df); i++ {
		set[strings.Join(df[i:i+n], " ")] = true
	}
	var hit int
	for i := 0; i+n <= len(st); i++ {
		if set[strings.Join(st[i:i+n], " ")] {
			hit++
		}
	}
	return float64(hit) / float64(total)
}

func tokenize(s string) []string {
	m := wordRe.FindAllString(strings.ToLower(s), -1)
	if m == nil {
		return []string{}
	}
	return m
}

func sha256Hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func screenedDiffText(diff string) (string, []string) {
	var b strings.Builder
	ex := map[string]bool{}
	include := true
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			p := parseDiffPath(line)
			include = !strings.HasSuffix(p, "_test.go")
			if !include && p != "" {
				ex[p] = true
			}
			continue
		}
		if !include || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			b.WriteString(line[1:])
			b.WriteByte('\n')
		}
	}
	out := make([]string, 0, len(ex))
	for p := range ex {
		out = append(out, p)
	}
	sort.Strings(out)
	return b.String(), out
}

func parseDiffPath(line string) string {
	parts := strings.Fields(line)
	if len(parts) < 4 {
		return ""
	}
	p := strings.TrimPrefix(parts[3], "b/")
	return filepath.ToSlash(p)
}

func leakScreenInstance(ctx context.Context, inst *Instance, goldDir string, threshold float64, now string) *Rejection {
	ls, err := RunLeakScreen(ctx, *inst, goldDir, threshold, now)
	if err != nil {
		return rejection(*inst, "leak-screen", "error", err.Error())
	}
	inst.Validation.LeakScreen = ls
	if !ls.Passed {
		return rejection(*inst, "leak-screen", "possible-fix-leak", fmt.Sprintf("score %.4f > threshold %.4f", ls.Score, ls.Threshold))
	}
	return nil
}
