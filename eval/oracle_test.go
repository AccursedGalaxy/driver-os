package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// sbAt builds a local sandbox over a temp dir seeded with files.
func sbAt(t *testing.T, files map[string]string) (sandbox.Sandbox, string) {
	t.Helper()
	dir := t.TempDir()
	if err := MapFixture(files).Materialize(dir); err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	return sb, dir
}

func TestCommandOraclePassFail(t *testing.T) {
	sb, dir := sbAt(t, nil)
	in := GradeInput{Root: dir, Sandbox: sb}

	if g := (CommandOracle{Cmd: "true"}).Grade(context.Background(), in); !g.Pass {
		t.Errorf("`true` should pass, got %+v", g)
	}
	g := CommandOracle{Label: "build", Cmd: "false"}.Grade(context.Background(), in)
	if g.Pass {
		t.Errorf("`false` should fail, got %+v", g)
	}
	if !strings.HasPrefix(g.Detail, "build: ") {
		t.Errorf("Detail should carry the label, got %q", g.Detail)
	}
}

func TestHeldOutDropsThenVerifies(t *testing.T) {
	// The run leaves base.txt; the held-out step drops extra.txt and the verify
	// command only passes when BOTH are present (proving the drop happened).
	sb, dir := sbAt(t, map[string]string{"base.txt": "x"})
	o := HeldOut{
		Drop:   MapFixture{"extra.txt": "y"},
		Verify: "test -f base.txt && test -f extra.txt",
	}
	if g := o.Grade(context.Background(), GradeInput{Root: dir, Sandbox: sb}); !g.Pass {
		t.Errorf("held-out should pass once dropped, got %+v", g)
	}
}

func TestAllOfShortCircuitsOnFirstFailure(t *testing.T) {
	sb, dir := sbAt(t, nil)
	in := GradeInput{Root: dir, Sandbox: sb}

	// First oracle fails -> AllOf fails with its Detail, later oracles irrelevant.
	o := AllOf(
		CommandOracle{Label: "suite", Cmd: "false"},
		CommandOracle{Label: "heldout", Cmd: "true"},
	)
	g := o.Grade(context.Background(), in)
	if g.Pass {
		t.Fatalf("AllOf should fail when the first oracle fails, got %+v", g)
	}
	if !strings.HasPrefix(g.Detail, "suite: ") {
		t.Errorf("Detail should be the FIRST failure's, got %q", g.Detail)
	}

	if g := AllOf(CommandOracle{Cmd: "true"}, CommandOracle{Cmd: "true"}).Grade(context.Background(), in); !g.Pass {
		t.Errorf("AllOf of two passers should pass, got %+v", g)
	}
}
