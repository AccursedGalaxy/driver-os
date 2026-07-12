package headless

import (
	"flag"
	"testing"

	"github.com/AccursedGalaxy/driver-os/cli"
)

// TestUsageGroupsComplete pins the -help grouping to the real flag set: every
// defined flag must be claimed by a group (no invisible "Other" flags), and
// every grouped name must still exist (no stale entries after a flag rename).
func TestUsageGroupsComplete(t *testing.T) {
	fs := flag.NewFlagSet("driver-agent", flag.ContinueOnError)
	registerFlags(fs)
	missing, stale := cli.UncategorizedFlags(fs, usageGroups)
	if len(missing) > 0 {
		t.Errorf("flags defined but not in any usage group (add them to usageGroups): %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("usage groups name flags that no longer exist: %v", stale)
	}
}
