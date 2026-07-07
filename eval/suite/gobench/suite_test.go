package gobench

import (
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
)

func TestLoadDefaultInstances(t *testing.T) {
	insts, err := Load(DefaultInstancesDir())
	if err != nil {
		t.Fatalf("Load(DefaultInstancesDir): %v", err)
	}
	if len(insts) != 5 {
		t.Fatalf("Load len = %d, want 5", len(insts))
	}
}

func TestCasesDefault(t *testing.T) {
	insts, err := Load(DefaultInstancesDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases, err := Cases(agent.Config{}, Opts{})
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	if len(cases) != 5 {
		t.Fatalf("Cases len = %d, want 5", len(cases))
	}
	byID := make(map[string]Instance, len(insts))
	for _, inst := range insts {
		byID[inst.InstanceID] = inst
	}
	for _, c := range cases {
		if !strings.HasPrefix(c.Name, "gobench-") {
			t.Fatalf("case name %q does not have gobench- prefix", c.Name)
		}
		inst, ok := byID[strings.TrimPrefix(c.Name, "gobench-")]
		if !ok {
			t.Fatalf("case %q not backed by a default instance", c.Name)
		}
		if c.Name != "gobench-"+inst.InstanceID {
			t.Fatalf("case name = %q, want gobench-%s", c.Name, inst.InstanceID)
		}
		if !c.VCSWorkspace {
			t.Fatalf("%s VCSWorkspace = false, want true", c.Name)
		}
		if c.Task == "" {
			t.Fatalf("%s Task is empty", c.Name)
		}
		snippet := strings.TrimSpace(inst.ProblemStatement)
		if len(snippet) > 80 {
			snippet = snippet[:80]
		}
		if !strings.Contains(c.Task, snippet) {
			t.Fatalf("%s Task does not contain problem statement snippet %q", c.Name, snippet)
		}
		if c.Oracle == nil {
			t.Fatalf("%s Oracle is nil", c.Name)
		}
	}
}

func TestCasesFilterByIDAndAlias(t *testing.T) {
	cases, err := Cases(agent.Config{}, Opts{IDs: []string{"opa-8781", "prometheus__prometheus-19013"}})
	if err != nil {
		t.Fatalf("Cases with IDs: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("Cases len = %d, want 2", len(cases))
	}
	if got, want := cases[0].Name, "gobench-open-policy-agent__opa-8781"; got != want {
		t.Fatalf("alias case[0] = %q, want %q", got, want)
	}
	if got, want := cases[1].Name, "gobench-prometheus__prometheus-19013"; got != want {
		t.Fatalf("full-id case[1] = %q, want %q", got, want)
	}
	if _, err := Cases(agent.Config{}, Opts{IDs: []string{"does-not-exist"}}); err == nil {
		t.Fatal("unknown id must error")
	}
}

func TestLadderVerifyCmd(t *testing.T) {
	insts, err := Load(DefaultInstancesDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var inst Instance
	for _, in := range insts {
		if len(in.PassToPass.Packages) > 0 {
			inst = in
			break
		}
	}
	if inst.InstanceID == "" {
		t.Fatal("no default instance has PASS_TO_PASS packages")
	}
	cmd := ladderVerifyCmd(inst)
	if cmd == "" {
		t.Fatal("ladderVerifyCmd is empty")
	}
	if !strings.Contains(cmd, "go test") {
		t.Fatalf("ladderVerifyCmd = %q, want to contain go test", cmd)
	}
	if !strings.Contains(cmd, inst.PassToPass.Packages[0]) {
		t.Fatalf("ladderVerifyCmd = %q, want to contain package %q", cmd, inst.PassToPass.Packages[0])
	}
}
