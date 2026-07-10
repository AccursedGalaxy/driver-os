package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyBaselineCacheReusesUnchangedTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	sb, root := setupGitRepo(t, map[string]string{"tracked.txt": "original\n"})
	cache := &verifyBaselineCache{}
	cfg := Config{
		Sandbox:             sb,
		Root:                root,
		VerifyCmd:           "echo x >> .git/verify-baseline-count; false",
		Obs:                 nopObserver{},
		verifyBaselineCache: cache,
	}

	first := &gates{cfg: cfg, runTimeout: defaultRunTimeout}
	first.measureVerifyBaseline(context.Background())
	if !first.verifyBaselineMeasured || !first.verifyBaselineRed {
		t.Fatalf("first baseline measured=%v red=%v, want both true", first.verifyBaselineMeasured, first.verifyBaselineRed)
	}

	second := &gates{cfg: cfg, runTimeout: defaultRunTimeout}
	second.measureVerifyBaseline(context.Background())
	if !second.verifyBaselineMeasured || !second.verifyBaselineRed {
		t.Fatalf("cached baseline measured=%v red=%v, want both true", second.verifyBaselineMeasured, second.verifyBaselineRed)
	}
	if second.verifyBaselineMeasured != first.verifyBaselineMeasured || second.verifyBaselineRed != first.verifyBaselineRed {
		t.Errorf("cached baseline fields measured=%v red=%v, want measured=%v red=%v", second.verifyBaselineMeasured, second.verifyBaselineRed, first.verifyBaselineMeasured, first.verifyBaselineRed)
	}

	countPath := filepath.Join(root, ".git", "verify-baseline-count")
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read verify command count: %v", err)
	}
	if got := strings.Count(string(count), "x\n"); got != 1 {
		t.Fatalf("verify command executions after unchanged tree = %d, want 1", got)
	}

	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatalf("modify tracked file: %v", err)
	}
	third := &gates{cfg: cfg, runTimeout: defaultRunTimeout}
	third.measureVerifyBaseline(context.Background())

	count, err = os.ReadFile(countPath)
	if err != nil {
		t.Fatalf("read verify command count after modification: %v", err)
	}
	if got := strings.Count(string(count), "x\n"); got != 2 {
		t.Errorf("verify command executions after tree change = %d, want 2", got)
	}
}
