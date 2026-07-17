package headless

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Setup errors must write the -report file natively (backlog 2026-07-10:
// "failSetup (exit-1) bypasses -report — delegate.sh synthesizes one; the
// binary should write it natively"). The orchestrator's one-read contract is
// the report file; a setup error that leaves it missing breaks that contract.
// Mirrors the configureSetupLedger pattern: run() arms the path once after
// flag parsing; failSetup consumes it.

func TestFailSetupWritesReport(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.md")
	configureSetupReport(reportPath)
	defer configureSetupReport("")
	var stdout, stderr strings.Builder

	code := failSetup(&stdout, &stderr, formatText, "invalid_flags", "boom went the flags")
	if code != 1 {
		t.Fatalf("failSetup exit = %d, want 1", code)
	}

	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("setup error did not write the report file: %v", err)
	}
	s := string(b)
	for _, want := range []string{"exit=1", "cli_error", "invalid_flags", "boom went the flags"} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q; got:\n%s", want, s)
		}
	}
}

func TestFailSetupNoReportPathWritesNothing(t *testing.T) {
	dir := t.TempDir()
	configureSetupReport("")
	var stdout, stderr strings.Builder

	code := failSetup(&stdout, &stderr, formatText, "trust", "no trust")
	if code != 1 {
		t.Fatalf("failSetup exit = %d, want 1", code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("no report path armed, but files were written: %v", entries)
	}
}
