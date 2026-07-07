package gobench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Scrubber is the seam between validation and the LLM-backed issue scrubber.
type Scrubber interface {
	Scrub(ctx context.Context, raw string) (string, error)
}

type LLMScrubber struct {
	Provider llm.Provider
	Model    string
}

func (s LLMScrubber) Scrub(ctx context.Context, raw string) (string, error) {
	if s.Provider == nil {
		return "", fmt.Errorf("scrub provider is nil")
	}
	resp, err := s.Provider.Generate(ctx, llm.Request{
		Model:     s.Model,
		System:    "You scrub Go issue reports for benchmark problem statements. Remove fix hints, PR/commit links, and stack-trace lines that identify the fix location. Keep verbatim symptom text and expected invariants. Return only the scrubbed statement.",
		Messages:  []llm.Message{llm.User(raw)},
		MaxTokens: 4096,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Text()), nil
}

func scrubProblemStatement(ctx context.Context, inst *Instance, raw string, scrubber Scrubber, outDir string) error {
	if scrubber == nil {
		return fmt.Errorf("scrubber is nil")
	}
	scrubbed, err := scrubber.Scrub(ctx, raw)
	if err != nil {
		return err
	}
	if strings.TrimSpace(scrubbed) == "" {
		return fmt.Errorf("scrubbed statement is empty")
	}
	if outDir != "" {
		if err := os.MkdirAll(filepath.Join(outDir, "raw-statements"), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(outDir, "scrub-diffs"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, "raw-statements", inst.InstanceID+".txt"), []byte(raw), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, "scrub-diffs", inst.InstanceID+".diff"), []byte(simpleUnifiedDiff("raw", "scrubbed", raw, scrubbed)), 0o644); err != nil {
			return err
		}
	}
	inst.ProblemStatement = scrubbed
	return nil
}

func simpleUnifiedDiff(a, b, old, new string) string {
	var sb strings.Builder
	sb.WriteString("--- " + a + "\n+++ " + b + "\n")
	for _, l := range strings.Split(strings.TrimSuffix(old, "\n"), "\n") {
		sb.WriteString("-" + l + "\n")
	}
	for _, l := range strings.Split(strings.TrimSuffix(new, "\n"), "\n") {
		sb.WriteString("+" + l + "\n")
	}
	return sb.String()
}
