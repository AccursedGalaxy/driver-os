package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

func TestGateInvariants_NeverGrindUnsatisfiableGate(t *testing.T) {
	t.Run("BaselineIdenticalFailureGetsOneGraceRoundThenTerminates", func(t *testing.T) {
		const failOut = "FAIL: invariant baseline red\n"
		for _, loop := range conformanceLoops(
			[]string{"answer done", "answer still done"},
			[][]llm.ContentPart{{llm.Text("done")}, {llm.Text("still done")}},
		) {
			t.Run(loop.name, func(t *testing.T) {
				base := sbWith(t, nil)
				sb := &scriptedExecSandbox{Sandbox: base, t: t, results: []*sandbox.Result{
					{Stdout: []byte(failOut), ExitCode: 1},
					{Stdout: []byte(failOut), ExitCode: 1},
					{Stdout: []byte(failOut), ExitCode: 1},
				}}
				cfg := conformanceBaseConfig(t)
				cfg.Sandbox = sb
				cfg.VerifyCmd = "go test ./..."
				cfg.VerifyContinue = true
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, Unverified, "ALREADY failing")
				if got := providerCallCount(rec); got != 2 {
					t.Fatalf("provider calls = %d, want 2 (one grace repair turn, then terminal)", got)
				}
				if sb.call != 3 {
					t.Fatalf("verify Exec calls = %d, want 3 (baseline + two finish checks)", sb.call)
				}
			})
		}
	})

	t.Run("TimedOutVerifyContinueDoesNotFeedContinue", func(t *testing.T) {
		for _, loop := range conformanceLoops([]string{"answer done"}, [][]llm.ContentPart{{llm.Text("done")}}) {
			t.Run(loop.name, func(t *testing.T) {
				base := sbWith(t, nil)
				sb := &scriptedExecSandbox{Sandbox: base, t: t, results: []*sandbox.Result{{TimedOut: true, ExitCode: 124}}}
				cfg := conformanceBaseConfig(t)
				cfg.Sandbox = sb
				cfg.VerifyCmd = "go test ./..."
				cfg.VerifyTimeout = time.Millisecond
				cfg.VerifyContinue = true
				cfg.SkipVerifyBaseline = true
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, Unverified, "timed out")
				if got := providerCallCount(rec); got != 1 {
					t.Fatalf("provider calls = %d, want 1 (timeout is inconclusive; no continue round)", got)
				}
			})
		}
	})
}

func TestGateInvariants_TimedOutVerifyIsNotFailed(t *testing.T) {
	for _, verifyContinue := range []bool{false, true} {
		name := "VerifyContinueFalse"
		if verifyContinue {
			name = "VerifyContinueTrue"
		}
		t.Run(name, func(t *testing.T) {
			for _, loop := range conformanceLoops([]string{"answer done"}, [][]llm.ContentPart{{llm.Text("done")}}) {
				t.Run(loop.name, func(t *testing.T) {
					base := sbWith(t, nil)
					sb := &scriptedExecSandbox{Sandbox: base, t: t, results: []*sandbox.Result{{TimedOut: true, ExitCode: 124}}}
					cfg := conformanceBaseConfig(t)
					cfg.Sandbox = sb
					cfg.VerifyCmd = "go test ./..."
					cfg.VerifyTimeout = time.Millisecond
					cfg.VerifyContinue = verifyContinue
					cfg.SkipVerifyBaseline = true
					res, rec, err := loop.run(context.Background(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					assertOutcome(t, res, Unverified, "timed out")
					if strings.Contains(res.Reason, "did not pass") || strings.Contains(res.Reason, "verify failed") {
						t.Fatalf("Reason = %q, want timeout/inconclusive wording, not failed wording", res.Reason)
					}
					if got := providerCallCount(rec); got != 1 {
						t.Fatalf("provider calls = %d, want 1", got)
					}
				})
			}
		})
	}
}

func TestGateInvariants_SafetyOnEveryFinishPath(t *testing.T) {
	t.Run("TestFenceAnswerPath", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"run printf changed > x_test.go", "answer done"},
			[][]llm.ContentPart{
				{structuredCall("c1", "run", map[string]any{"command": "printf changed > x_test.go"})},
				{llm.Text("done")},
			},
		) {
			t.Run(loop.name, func(t *testing.T) {
				sb := sbWith(t, map[string]string{"x_test.go": "original\n"})
				cfg := conformanceBaseConfig(t)
				cfg.Sandbox = sb
				cfg.TestFence = []string{"*_test.go"}
				res, _, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, Unverified, "test fence violated")
			})
		}
	})

	t.Run("TestFenceFinishTool", func(t *testing.T) {
		for _, trusted := range []bool{false, true} {
			name := "Untrusted"
			if trusted {
				name = "Trusted"
			}
			t.Run(name, func(t *testing.T) {
				sb := sbWith(t, map[string]string{"x_test.go": "original\n"})
				cfg := conformanceBaseConfig(t)
				cfg.Sandbox = sb
				cfg.Tools = finishToolsForSandbox(t, sb)
				cfg.FinishTool = "say"
				cfg.FinishToolTrustsCaller = trusted
				cfg.TestFence = []string{"*_test.go"}
				cfg.Model = &nativeScript{turns: [][]llm.ContentPart{{
					structuredCall("c1", "run", map[string]any{"command": "printf changed > x_test.go"}),
					finishCall("f1", map[string]any{"message": "done"}),
				}}}
				res, err := RunNative(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, Unverified, "test fence violated")
			})
		}
	})

	t.Run("DiffScopeAnswerPath", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"run printf bad > outside.txt", "answer done"},
			[][]llm.ContentPart{
				{structuredCall("c1", "run", map[string]any{"command": "printf bad > outside.txt"})},
				{llm.Text("done")},
			},
		) {
			t.Run(loop.name, func(t *testing.T) {
				sb, root := gitWorkspace(t, map[string]string{"allowed/keep.txt": "ok\n"})
				cfg := conformanceBaseConfig(t)
				cfg.Sandbox = sb
				cfg.Root = root
				cfg.DiffScope = []string{"allowed/**"}
				res, _, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, ScopeViolation, "diff scope violated")
			})
		}
	})

	t.Run("DiffScopeFinishTool", func(t *testing.T) {
		for _, trusted := range []bool{false, true} {
			name := "Untrusted"
			if trusted {
				name = "Trusted"
			}
			t.Run(name, func(t *testing.T) {
				sb, root := gitWorkspace(t, map[string]string{"allowed/keep.txt": "ok\n"})
				cfg := conformanceBaseConfig(t)
				cfg.Sandbox = sb
				cfg.Root = root
				cfg.Tools = finishToolsForSandbox(t, sb)
				cfg.FinishTool = "say"
				cfg.FinishToolTrustsCaller = trusted
				cfg.VerifyCmd = "false"
				cfg.DiffScope = []string{"allowed/**"}
				cfg.Model = &nativeScript{turns: [][]llm.ContentPart{{
					structuredCall("c1", "run", map[string]any{"command": "printf bad > outside.txt"}),
					finishCall("f1", map[string]any{"message": "done"}),
				}}}
				res, err := RunNative(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, ScopeViolation, "diff scope violated")
			})
		}
	})
}

func TestGateInvariants_ScopeViolationIsTerminalRegardlessOfVerifyContinue(t *testing.T) {
	for _, loop := range conformanceLoops(
		[]string{"run printf bad > outside.txt", "answer done"},
		[][]llm.ContentPart{
			{structuredCall("c1", "run", map[string]any{"command": "printf bad > outside.txt"})},
			{llm.Text("done")},
		},
	) {
		t.Run(loop.name, func(t *testing.T) {
			sb, root := gitWorkspace(t, map[string]string{"allowed/keep.txt": "ok\n"})
			cfg := conformanceBaseConfig(t)
			cfg.Sandbox = sb
			cfg.Root = root
			cfg.DiffScope = []string{"allowed/**"}
			cfg.VerifyContinue = true
			res, rec, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, ScopeViolation, "diff scope violated")
			if got := providerCallCount(rec); got != 2 {
				t.Fatalf("provider calls = %d, want 2 (mutation turn + finish turn; no continue round)", got)
			}
		})
	}
}

func TestGateInvariants_AbortOnRedBaselinePrecedesEverything(t *testing.T) {
	for _, loop := range conformanceLoops([]string{"answer should not run"}, [][]llm.ContentPart{{llm.Text("should not run")}}) {
		t.Run(loop.name, func(t *testing.T) {
			base := sbWith(t, nil)
			sb := &scriptedExecSandbox{Sandbox: base, t: t, results: []*sandbox.Result{{Stderr: []byte("baseline red\n"), ExitCode: 1}}}
			cfg := conformanceBaseConfig(t)
			cfg.Sandbox = sb
			cfg.VerifyCmd = "go test ./..."
			cfg.AbortOnRedBaseline = true
			cfg.VerifyLastRun = true
			res, rec, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "refused to run")
			if got := providerCallCount(rec); got != 0 {
				t.Fatalf("provider calls = %d, want 0", got)
			}
			if sb.call != 1 {
				t.Fatalf("verify Exec calls = %d, want 1 baseline check only", sb.call)
			}
		})
	}
}

func TestGateInvariants_ReviewerVerifyContinueStopsAtReviewRounds(t *testing.T) {
	for _, loop := range conformanceLoops(
		[]string{"write_file calc.go package calc // patched", "answer done", "answer still done"},
		[][]llm.ContentPart{
			{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc // patched\n"})},
			{llm.Text("done")},
			{llm.Text("still done")},
		},
	) {
		t.Run(loop.name, func(t *testing.T) {
			rv := &fakeReviewer{verdicts: [][]ReviewFinding{
				{blocker("calc.go", "package calc // patched")},
				{blocker("calc.go", "package calc // patched")},
			}}
			sb, root := gitWorkspace(t, map[string]string{"calc.go": "package calc\n"})
			cfg := conformanceBaseConfig(t)
			cfg.Sandbox = sb
			cfg.Root = root
			cfg.VerifyCmd = "true"
			cfg.SkipVerifyBaseline = true
			cfg.VerifyContinue = true
			cfg.Reviewer = rv
			cfg.ReviewRounds = 2
			res, rec, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "review blockers remain after 2 round(s)")
			if got := len(rv.inputs); got != 2 {
				t.Fatalf("reviewer calls = %d, want 2", got)
			}
			if got := providerCallCount(rec); got != 3 {
				t.Fatalf("provider calls = %d, want 3 (edit + first finish + one repair finish)", got)
			}
		})
	}
}

func TestGateInvariants_FenceViolationDoesNotGrindWithVerifyContinue(t *testing.T) {
	for _, loop := range conformanceLoops(
		[]string{"run printf changed > x_test.go", "answer done"},
		[][]llm.ContentPart{
			{structuredCall("c1", "run", map[string]any{"command": "printf changed > x_test.go"})},
			{llm.Text("done")},
		},
	) {
		t.Run(loop.name, func(t *testing.T) {
			sb := sbWith(t, map[string]string{"x_test.go": "original\n"})
			cfg := conformanceBaseConfig(t)
			cfg.Sandbox = sb
			cfg.TestFence = []string{"*_test.go"}
			cfg.VerifyContinue = true
			res, rec, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "test fence violated")
			if got := providerCallCount(rec); got != 2 {
				t.Fatalf("provider calls = %d, want 2 (mutation turn + finish turn; no continue round)", got)
			}
		})
	}
}

func finishToolsForSandbox(t *testing.T, sb sandbox.Sandbox) map[string]Tool {
	t.Helper()
	tools := DefaultTools(sb, time.Second)
	tools["say"] = Tool{
		Name:       "say",
		Desc:       "say the final message",
		NativeDesc: "send the final message",
		Run: func(context.Context, string) (string, error) {
			return "", nil
		},
	}
	return tools
}
