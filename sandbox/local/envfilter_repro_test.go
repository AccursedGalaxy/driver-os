package local

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Oracle for PROFILES.md S2b: the local sandbox must support a clean-env
// allowlist so reviewed-local's secrets floor (SecretsAllowlist) is real.
// Today local.go unconditionally inherits the full ambient environment —
// handing every model-authored command the operator's API keys.
func TestExecEnvAllowlist(t *testing.T) {
	t.Setenv("DRIVER_TEST_SECRET", "s3cret-value")
	dir := t.TempDir()

	// Control: the plain constructor keeps today's ambient behavior.
	plain, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := plain.Exec(context.Background(), sandbox.Command{Path: "env"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("control env: %v exit=%d", err, res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "DRIVER_TEST_SECRET=") {
		t.Fatal("control: plain New must keep ambient env (behavior unchanged for trusted-local)")
	}

	// Allowlisted: only named variables cross into the command env.
	filtered, err := NewWithOptions(dir, Options{EnvAllowlist: []string{"PATH", "HOME"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err = filtered.Exec(context.Background(), sandbox.Command{Path: "env"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("filtered env: %v exit=%d", err, res.ExitCode)
	}
	out := string(res.Stdout)
	if strings.Contains(out, "DRIVER_TEST_SECRET=") {
		t.Fatal("allowlisted sandbox leaked a non-allowlisted variable")
	}
	if !strings.Contains(out, "PATH=") {
		t.Fatal("allowlisted sandbox must pass allowlisted variables through")
	}

	// Per-command Env stays additive over the filtered base (a command may
	// still receive explicit harness-supplied vars).
	res, err = filtered.Exec(context.Background(), sandbox.Command{Path: "env", Env: []string{"EXPLICIT_VAR=yes"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("explicit env: %v exit=%d", err, res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "EXPLICIT_VAR=yes") {
		t.Fatal("per-command Env must still merge over the filtered base")
	}
}
