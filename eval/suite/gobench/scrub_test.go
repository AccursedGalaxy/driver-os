package gobench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeScrubber struct{ out string }

func (f fakeScrubber) Scrub(context.Context, string) (string, error) { return f.out, nil }

func TestScrubWritesArtifactsAndUpdatesStatement(t *testing.T) {
	inst := Instance{InstanceID: "x"}
	dir := t.TempDir()
	err := scrubProblemStatement(context.Background(), &inst, "raw fix hint", fakeScrubber{"raw symptom"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if inst.ProblemStatement != "raw symptom" {
		t.Fatalf("statement=%q", inst.ProblemStatement)
	}
	if _, err := os.Stat(filepath.Join(dir, "raw-statements", "x.txt")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "scrub-diffs", "x.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "-raw fix hint") || !strings.Contains(string(b), "+raw symptom") {
		t.Fatalf("bad diff: %s", b)
	}
}
