//go:build docker_integration

package agent

import (
	"context"
	"os/exec"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox/docker"
)

// TestDockerNetworkFloorAdmission verifies that the runtime floor consumes the
// docker backend's observed capability, not merely the requested Options.
func TestDockerNetworkFloorAdmission(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("SKIP: docker CLI not found — run `make sandbox-image` on a host with docker")
	}
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Skipf("SKIP: docker daemon not reachable (%v): %s", err, out)
	}
	if err := exec.Command("docker", "image", "inspect", docker.DefaultImage).Run(); err != nil {
		t.Skipf("SKIP: image %s not built — run `make sandbox-image`", docker.DefaultImage)
	}
	sb, err := docker.New(context.Background(), t.TempDir(), docker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	if sb.Capabilities().Network {
		t.Fatal("docker Options{} must advertise network off")
	}
	res, err := runT(context.Background(), Config{Model: &scripted{replies: []string{"answer done"}}, Sandbox: sb, Task: "x", RequireNetworkOff: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, Answered, res.Reason)
	}
}
