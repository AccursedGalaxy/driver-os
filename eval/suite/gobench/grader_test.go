package gobench

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrade_DoltCollision_ResetsAgentTestEdits(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, repo, "calc.go", `package example

func Add(a, b int) int { return a - b }
`)
	// The agent leaves an untracked fake/colliding test behind. Without the reset
	// and clean step, this redeclares the oracle's TestOracle and testbuild fails.
	writeFile(t, repo, "fake_test.go", `package example

import "testing"

func TestOracle(t *testing.T) {}
`)
	oracleDir := t.TempDir()
	writeFile(t, oracleDir, "oracle_test.go", `package example

import "testing"

func TestOracle(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("oracle saw buggy Add")
	}
}
`)

	v, err := Grade(repo, oracleDir, baseInstance([]string{"oracle_test.go"}, []OracleTest{{
		TestID:   TestID{Package: ".", Name: "TestOracle"},
		RunRegex: "^TestOracle$",
	}}, nil))
	if err != nil {
		t.Fatalf("Grade returned error: %v", err)
	}
	if !v.TestbuildOK {
		t.Fatalf("TestbuildOK=false, reset did not prevent collision: %#v", v)
	}
	if v.F2PPass || v.Resolved {
		t.Fatalf("fake agent test was credited instead of failing oracle: %#v", v)
	}
	if len(v.RanTests) != 1 || v.RanTests[0].Name != "TestOracle" {
		t.Fatalf("ran tests proof = %#v, want TestOracle", v.RanTests)
	}
}

func TestGrade_RunMiss_NotAFalsePass(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, repo, "calc.go", `package example

func Add(a, b int) int { return a + b }
`)
	oracleDir := t.TempDir()
	writeFile(t, oracleDir, "oracle_test.go", `package example

import "testing"

func TestDifferent(t *testing.T) {}
`)

	v, err := Grade(repo, oracleDir, baseInstance([]string{"oracle_test.go"}, []OracleTest{{
		TestID:   TestID{Package: ".", Name: "TestMissing"},
		RunRegex: "^TestMissing$",
	}}, nil))
	if err != nil {
		t.Fatalf("Grade returned error: %v", err)
	}
	if !v.TestbuildOK {
		t.Fatalf("TestbuildOK=false: %#v", v)
	}
	if v.F2PPass || v.Resolved {
		t.Fatalf("-run miss scored as pass: %#v", v)
	}
	if !strings.Contains(v.GraderError, "did not run") {
		t.Fatalf("GraderError = %q, want run-miss reason", v.GraderError)
	}
}

func TestGrade_HappyPath(t *testing.T) {
	repo := newFixtureRepo(t)
	// Simulate the agent fixing the buggy base checkout before grading.
	writeFile(t, repo, "calc.go", `package example

func Add(a, b int) int { return a + b }
`)
	oracleDir := t.TempDir()
	writeFile(t, oracleDir, "oracle_test.go", `package example

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add is still broken")
	}
}
`)

	v, err := Grade(repo, oracleDir, baseInstance([]string{"oracle_test.go"}, []OracleTest{{
		TestID:   TestID{Package: ".", Name: "TestAdd"},
		RunRegex: "^TestAdd$",
	}}, []string{"."}))
	if err != nil {
		t.Fatalf("Grade returned error: %v", err)
	}
	if !v.TestbuildOK || !v.F2PPass || !v.P2PPass || !v.Resolved {
		t.Fatalf("happy path did not resolve: %#v", v)
	}
}

func TestGrade_P2PRegression(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, repo, "stable/stable.go", `package stable

func Stable() bool { return false }
`)
	writeFile(t, repo, "calc.go", `package example

func Add(a, b int) int { return a + b }
`)
	oracleDir := t.TempDir()
	writeFile(t, oracleDir, "oracle_test.go", `package example

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add is still broken")
	}
}
`)

	v, err := Grade(repo, oracleDir, baseInstance([]string{"oracle_test.go"}, []OracleTest{{
		TestID:   TestID{Package: ".", Name: "TestAdd"},
		RunRegex: "^TestAdd$",
	}}, []string{"./stable"}))
	if err != nil {
		t.Fatalf("Grade returned error: %v", err)
	}
	if !v.TestbuildOK || !v.F2PPass {
		t.Fatalf("F2P should pass before checking P2P regression: %#v", v)
	}
	if v.P2PPass || v.Resolved {
		t.Fatalf("P2P regression scored as resolved: %#v", v)
	}
}

func newFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runCmd(t, dir, "git", "init")
	writeFile(t, dir, "go.mod", "module example.test\n\ngo 1.22\n")
	writeFile(t, dir, "calc.go", `package example

func Add(a, b int) int { return a - b }
`)
	writeFile(t, dir, "calc_test.go", `package example

import "testing"

func TestBase(t *testing.T) {}
`)
	writeFile(t, dir, "stable/stable.go", `package stable

func Stable() bool { return true }
`)
	writeFile(t, dir, "stable/stable_test.go", `package stable

import "testing"

func TestStable(t *testing.T) {
	if !Stable() {
		t.Fatalf("stable regressed")
	}
}
`)
	runCmd(t, dir, "git", "add", "-A")
	runCmd(t, dir, "git", "-c", "user.name=GoBench Test", "-c", "user.email=gobench@example.test", "commit", "-m", "base")
	return dir
}

func baseInstance(oracleFiles []string, f2p []OracleTest, p2p []string) Instance {
	return Instance{
		SchemaVersion: "gobench.instance.v1",
		InstanceID:    "synthetic__fixture-1",
		ModuleDir:     ".",
		OracleFiles:   oracleFiles,
		FailToPass:    f2p,
		PassToPass:    PassToPass{Packages: p2p},
		Exec:          ExecSpec{Cwd: ".", Argv: []string{"go", "test"}},
		TestTimeout:   "30s",
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}
