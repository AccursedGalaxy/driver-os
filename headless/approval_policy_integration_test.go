package headless

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/gated"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// This exercises the production chain after the run tool has constructed its
// sh -c Command: profile adapter -> gated sandbox -> host-local Exec.
func TestDeniedShellBypassNeverReachesSandboxOrFilesystem(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "pwned")
	inner, err := local.New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	policy, err := profile.PolicyByName(profile.DefaultHeadlessPolicy)
	if err != nil {
		t.Fatal(err)
	}
	gate := gated.New(inner, nil, approvalPolicyAdapter(policy))

	command := `go test "$(touch ` + outside + `)"`
	res, err := gate.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", command}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 126 {
		t.Fatalf("denied bypass exit = %d, want 126", res.ExitCode)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("denied substitution reached host filesystem: stat err=%v", err)
	}
}
