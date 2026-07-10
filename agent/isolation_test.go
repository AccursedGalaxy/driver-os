package agent

import (
	"context"
	"io/fs"
	"iter"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// failIfCalled is an llm.Provider that fails the test if the loop ever calls it.
// It is the proof that the MinIsolation gate refuses BEFORE the first model call:
// a too-weak sandbox must never reach the model (and so never run hostile code).
type failIfCalled struct{ t *testing.T }

func (p failIfCalled) Name() string                   { return "fail-if-called" }
func (p failIfCalled) Capabilities() llm.Capabilities { return llm.Capabilities{Tools: true} }
func (p failIfCalled) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("fail-if-called")
}
func (p failIfCalled) Generate(context.Context, llm.Request) (*llm.Response, error) {
	p.t.Fatal("model was called despite a too-weak sandbox — the MinIsolation gate must refuse first")
	return nil, nil
}

// fakeIso is a no-op sandbox that advertises a configurable isolation level, so a
// test can hand the gate exactly the capability it wants to check against without
// standing up a real container.
type fakeIso struct{ iso sandbox.Isolation }

func (f fakeIso) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Isolation: f.iso}
}
func (f fakeIso) Exec(context.Context, sandbox.Command) (*sandbox.Result, error) {
	return &sandbox.Result{}, nil
}
func (f fakeIso) ReadFile(context.Context, string) ([]byte, error)             { return nil, nil }
func (f fakeIso) WriteFile(context.Context, string, []byte, fs.FileMode) error { return nil }
func (f fakeIso) ListDir(context.Context, string) ([]sandbox.DirEntry, error)  { return nil, nil }
func (f fakeIso) Close() error                                                 { return nil }

// TestRefusesTooWeakSandbox covers the gate for BOTH loops: a MinIsolation higher
// than the sandbox provides must yield RefusedUnsafe without ever calling the
// model — and without running a configured VerifyCmd (a refused run must not
// execute anything on the unsafe sandbox).
func TestRefusesTooWeakSandbox(t *testing.T) {
	for _, loop := range []struct {
		name string
		run  func(context.Context, Config) (*RunResult, error)
	}{
		{"text", Run},
		{"native", RunNative},
	} {
		t.Run(loop.name, func(t *testing.T) {
			lsb, err := local.New(t.TempDir()) // IsolationNone.
			if err != nil {
				t.Fatal(err)
			}
			res, err := loop.run(context.Background(), Config{
				Model:        failIfCalled{t},
				Sandbox:      lsb,
				Task:         "do something untrusted",
				MinIsolation: sandbox.IsolationKernel,
				VerifyCmd:    "this-must-never-run", // proves no verify on a refused run.
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != RefusedUnsafe {
				t.Errorf("Outcome = %q, want %q (Reason: %s)", res.Outcome, RefusedUnsafe, res.Reason)
			}
			if res.Answer != "" {
				t.Errorf("a refused run must have no answer, got %q", res.Answer)
			}
		})
	}
}

// TestNilSandboxFailsClosed proves the gate fails CLOSED: a nil Sandbox under a
// non-zero MinIsolation is a refusal, not a panic on Capabilities().
func TestNilSandboxFailsClosed(t *testing.T) {
	_, err := Run(context.Background(), Config{Model: failIfCalled{t}, Task: "x", MinIsolation: sandbox.IsolationKernel})
	if err == nil || !strings.Contains(err.Error(), "Sandbox") {
		t.Fatalf("nil sandbox should be rejected as invalid setup, got %v", err)
	}
}

// TestAdmitsStrongEnoughSandbox proves the gate is not over-eager: when the
// sandbox meets MinIsolation, the run proceeds to the model as normal.
func TestAdmitsStrongEnoughSandbox(t *testing.T) {
	sp := &scripted{replies: []string{"answer done"}}
	res, err := Run(context.Background(), Config{
		Model:        sp,
		Sandbox:      fakeIso{iso: sandbox.IsolationKernel},
		Task:         "x",
		MinIsolation: sandbox.IsolationKernel,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sp.calls) == 0 {
		t.Error("a sandbox meeting MinIsolation must let the run reach the model")
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q, want answered (Reason: %s)", res.Outcome, res.Reason)
	}
}

// TestDefaultMinIsolationAdmitsLocal proves the zero value preserves today's
// behavior: a trusted caller (issue-bot/eval) on a local sandbox is never
// refused.
func TestDefaultMinIsolationAdmitsLocal(t *testing.T) {
	res, sp := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"list_dir .", "answer x"})
	if res.Outcome == RefusedUnsafe {
		t.Fatal("default MinIsolation (0) must admit the local sandbox")
	}
	if len(sp.calls) == 0 {
		t.Error("default run should reach the model")
	}
}
