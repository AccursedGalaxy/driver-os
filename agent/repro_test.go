package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox/local"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

func reproRepo(t *testing.T, fence ...string) (string, *gates) {
	t.Helper()
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module reprofixture\n\ngo 1.23\n")
	write("calc.go", "package reprofixture\n\nfunc Add(a, b int) int { return a - b }\n")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@example.com")
	run("git", "config", "user.name", "Test")
	run("git", "add", ".")
	run("git", "commit", "-m", "base")
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	globs := []string{"*_test.go"}
	if len(fence) > 0 {
		globs = fence
	}
	g, err := newGates(context.Background(), Config{Sandbox: sb, VerifySandbox: sb, Root: root, ReproFirst: true, TestFence: globs, SkipVerifyBaseline: true}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return root, g
}

func TestGradeReproRed(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"go test fail", "exit 1\nstdout:\n--- FAIL: TestAdd\nFAIL\treprofixture\t0.01s", "test_failure"},
		{"fail tab", "exit 1\nstdout:\nFAIL\t./pkg\t0.01s", "test_failure"},
		{"build failed", "exit 1\nstderr:\n# reprofixture [reprofixture.test]\n./x_test.go:6: undefined: Nope\nFAIL\treprofixture [build failed]", "build_failure"},
		{"setup failed", "exit 1\nFAIL\t./missing [setup failed]\nno such file or directory", "build_failure"},
		{"unknown", "exit 1\nstdout:\nsome command failed", "unrecognized"},
		{"fail with generic error text", "exit 1\nstdout:\n--- FAIL: TestOpen\n    open /tmp/x: no such file or directory\nFAIL\treprofixture\t0.01s", "test_failure"},
		{"fail asserting compile-ish msg", "exit 1\nstdout:\n--- FAIL: TestMsg\n    got \"undefined: foo\" want \"\"\nFAIL", "test_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gradeReproRed(tc.out); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDeclareReproAcceptsLocksAndRestores(t *testing.T) {
	root, g := reproRepo(t)
	body := "package reprofixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatalf(\"bad\") } }\n"
	if err := os.WriteFile(filepath.Join(root, "calc_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := vcs.WriteTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.declareRepro(context.Background(), "calc_test.go", "go test -run TestAdd .")
	if err != nil {
		t.Fatalf("declare: %v\n%s", err, out)
	}
	after, err := vcs.WriteTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("workspace tree changed: before %s after %s", before, after)
	}
	if g.repro == nil || g.repro.report.Path != "calc_test.go" {
		t.Fatalf("repro not recorded: %#v", g.repro)
	}
	tools := g.addReproTools(wrapTools(g.cfg, time.Second))
	if _, err := tools["edit_file"].RunJSON(context.Background(), []byte(`{"path":"calc_test.go","old":"bad","new":"worse"}`)); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("locked edit got %v", err)
	}
	if _, err := tools["write_file"].RunJSON(context.Background(), []byte(`{"path":"other_test.go","content":"package reprofixture"}`)); err == nil || !strings.Contains(err.Error(), "test fence") {
		t.Fatalf("new fenced write after lock got %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "calc_test.go"), []byte(body+"// drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := g.fence.drift(context.Background(), g.cfg.Sandbox); len(d) == 0 {
		t.Fatalf("want fence drift")
	}
}

func TestDeclareReproRejectsGreenBuildFailureAndBasePath(t *testing.T) {
	for _, tc := range []struct{ name, path, body, cmd, want string }{
		{"green", "green_test.go", "package reprofixture\n\nimport \"testing\"\nfunc TestGreen(t *testing.T){}\n", "go test -run TestGreen .", "PASSES"},
		{"build", "build_test.go", "package reprofixture\n\nimport \"testing\"\nfunc TestBuild(t *testing.T){ Nope() }\n", "go test -run TestBuild .", "BUILD"},
		{"base", "calc.go", "package reprofixture\n", "go test .", "NEW file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, g := reproRepo(t)
			if err := os.WriteFile(filepath.Join(root, tc.path), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := g.declareRepro(context.Background(), tc.path, tc.cmd)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

// Declaring AFTER the fix is already in the working tree must still validate
// against the true base: the restore strips the fix, so the repro reds at
// base, and reproFinish then greens on the fixed tree.
func TestDeclareReproAfterFixValidatesAgainstTrueBase(t *testing.T) {
	root, g := reproRepo(t)
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package reprofixture\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "package reprofixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatalf(\"bad\") } }\n"
	if err := os.WriteFile(filepath.Join(root, "calc_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.declareRepro(context.Background(), "calc_test.go", "go test -run TestAdd ."); err != nil {
		t.Fatalf("declare after fix: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "calc.go")); err != nil || !strings.Contains(string(got), "a + b") {
		t.Fatalf("fix lost after validation round-trip: %v %q", err, got)
	}
	if fb, detail := g.reproFinish(context.Background()); fb != "" || detail != "" {
		t.Fatalf("green final fb=%q detail=%q", fb, detail)
	}
}

// A validated repro whose path does NOT match the fence globs must not be
// injected into the fence snapshot: drift() re-walks by glob and would report
// the entry as "deleted" — a spurious violation.
func TestDeclareReproFenceGlobMismatchNoSpuriousDrift(t *testing.T) {
	root, g := reproRepo(t, "testdata/**")
	body := "package reprofixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatalf(\"bad\") } }\n"
	if err := os.WriteFile(filepath.Join(root, "calc_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.declareRepro(context.Background(), "calc_test.go", "go test -run TestAdd ."); err != nil {
		t.Fatal(err)
	}
	if g.fence != nil {
		if _, ok := g.fence.base["calc_test.go"]; ok {
			t.Fatalf("non-glob-matching repro path injected into fence snapshot")
		}
		if d := g.fence.drift(context.Background(), g.cfg.Sandbox); len(d) != 0 {
			t.Fatalf("spurious fence drift: %v", d)
		}
	}
}

// repro_missing must feed back and continue even without -verify-continue —
// it is a protocol nudge, not a red gate. Out of budget it stops Unverified.
func TestFinishReproMissingContinuesWithoutVerifyContinue(t *testing.T) {
	_, g := reproRepo(t)
	if g.cfg.VerifyContinue {
		t.Fatal("fixture unexpectedly sets VerifyContinue")
	}
	d := g.finish(context.Background(), finishInput{answer: "done", canContinue: true, trusted: true})
	if d.kind != finishContinue || !strings.Contains(d.feedback, "declare_repro") {
		t.Fatalf("want continue with declare_repro nudge, got kind=%v feedback=%q", d.kind, d.feedback)
	}
	d = g.finish(context.Background(), finishInput{answer: "done", canContinue: false, trusted: true})
	if d.kind != finishStop || d.outcome != Unverified || d.reason != "repro_missing" {
		t.Fatalf("want Unverified repro_missing stop, got kind=%v outcome=%v reason=%q", d.kind, d.outcome, d.reason)
	}
}

func TestReproFinishAndSkip(t *testing.T) {
	root, g := reproRepo(t)
	body := "package reprofixture\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatalf(\"bad\") } }\n"
	if err := os.WriteFile(filepath.Join(root, "calc_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.declareRepro(context.Background(), "calc_test.go", "go test -run TestAdd ."); err != nil {
		t.Fatal(err)
	}
	if fb, detail := g.reproFinish(context.Background()); fb == "" || detail != "repro_red" {
		t.Fatalf("red final fb=%q detail=%q", fb, detail)
	}
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package reprofixture\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fb, detail := g.reproFinish(context.Background()); fb != "" || detail != "" {
		t.Fatalf("green final fb=%q detail=%q", fb, detail)
	}
	if g.repro.report.Green == nil || !*g.repro.report.Green {
		t.Fatalf("green not recorded")
	}
	// Tamper with the locked repro out-of-band: the finish-time sha256
	// re-check is the last-line defense against shell rewrites.
	if err := os.WriteFile(filepath.Join(root, "calc_test.go"), []byte(body+"// weakened\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fb, detail := g.reproFinish(context.Background()); detail != "repro_red" || !strings.Contains(fb, "changed after it was locked") {
		t.Fatalf("tampered repro fb=%q detail=%q", fb, detail)
	}
	_, g2 := reproRepo(t)
	if fb, detail := g2.reproFinish(context.Background()); fb == "" || detail != "repro_missing" {
		t.Fatalf("missing fb=%q detail=%q", fb, detail)
	}
	if _, err := g2.skipRepro("not executable"); err != nil {
		t.Fatal(err)
	}
	if fb, detail := g2.reproFinish(context.Background()); fb != "" || detail != "" {
		t.Fatalf("skip fb=%q detail=%q", fb, detail)
	}
}
