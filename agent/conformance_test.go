package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/mneme"
)

type conformanceLoop struct {
	name string
	run  func(context.Context, Config) (*RunResult, any, error)
}

func conformanceLoops(textReplies []string, nativeTurns [][]llm.ContentPart) []conformanceLoop {
	return []conformanceLoop{
		{name: "Run", run: func(ctx context.Context, cfg Config) (*RunResult, any, error) {
			sp := &scripted{replies: textReplies}
			cfg.Model = sp
			res, err := Run(ctx, cfg)
			return res, sp, err
		}},
		{name: "RunNative", run: func(ctx context.Context, cfg Config) (*RunResult, any, error) {
			ns := &nativeScript{turns: nativeTurns}
			cfg.Model = ns
			res, err := RunNative(ctx, cfg)
			return res, ns, err
		}},
	}
}

func conformanceBaseConfig(t *testing.T) Config {
	t.Helper()
	return Config{Sandbox: sbWith(t, nil), Task: "test task", MaxIterations: 5}
}

func assertOutcome(t *testing.T, res *RunResult, want Outcome, reasonSubstr string) {
	t.Helper()
	if res.Outcome != want {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, want, res.Reason)
	}
	if reasonSubstr != "" && !strings.Contains(res.Reason, reasonSubstr) {
		t.Fatalf("Reason = %q, want substring %q", res.Reason, reasonSubstr)
	}
}

func requestMessages(rec any, idx int) []llm.Message {
	switch r := rec.(type) {
	case *scripted:
		return r.calls[idx].Messages
	case *nativeScript:
		return r.calls[idx].Messages
	default:
		return nil
	}
}

func providerCallCount(rec any) int {
	switch r := rec.(type) {
	case *scripted:
		return len(r.calls)
	case *nativeScript:
		return len(r.calls)
	default:
		return -1
	}
}

func messagesContain(msgs []llm.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Text(), sub) {
			return true
		}
	}
	return false
}

func finishTools(t *testing.T) map[string]Tool {
	t.Helper()
	tools := DefaultTools(sbWith(t, nil), time.Second)
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

func finishCall(id string, fields map[string]any) llm.ToolCallPart {
	args, _ := json.Marshal(fields)
	return llm.ToolCallPart{ID: id, Name: "say", Args: args}
}

func TestConformance_VerifiedFinish(t *testing.T) {
	for _, loop := range conformanceLoops([]string{"answer done"}, [][]llm.ContentPart{{llm.Text("done")}}) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			cfg.VerifyCmd = "true"
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Answered, "")
			if res.Answer != "done" {
				t.Fatalf("Answer = %q, want done", res.Answer)
			}
		})
	}
}

func TestConformance_VerifyFailWithoutContinue(t *testing.T) {
	for _, loop := range conformanceLoops([]string{"answer done"}, [][]llm.ContentPart{{llm.Text("done")}}) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			cfg.VerifyCmd = "sh -c 'echo verify failed; exit 1'"
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "verify failed")
		})
	}
}

func TestConformance_VerifyContinueThenGreenFinish(t *testing.T) {
	for _, loop := range conformanceLoops(
		[]string{"answer premature", "write_file ok yes", "answer done"},
		[][]llm.ContentPart{
			{llm.Text("premature")},
			{structuredCall("c1", "write_file", map[string]any{"path": "ok", "content": "yes"})},
			{llm.Text("done")},
		},
	) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			cfg.VerifyCmd = "test -f ok"
			cfg.VerifyContinue = true
			res, rec, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Answered, "")
			if res.Answer != "done" {
				t.Fatalf("Answer = %q, want done", res.Answer)
			}
			if providerCallCount(rec) < 2 || !messagesContain(requestMessages(rec, 1), "Not finished") {
				t.Fatalf("second request did not contain the VerifyContinue Not finished observation")
			}
		})
	}
}

func TestConformance_EmptyFinalAnswer(t *testing.T) {
	for _, loop := range conformanceLoops([]string{"answer"}, [][]llm.ContentPart{{}}) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "empty final answer")
		})
	}
}

func TestConformance_BudgetStops(t *testing.T) {
	t.Run("iteration cap", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"ponder one", "ponder two"},
			[][]llm.ContentPart{{structuredCall("c1", "read_file", map[string]any{"path": "missing-a"})}, {structuredCall("c2", "read_file", map[string]any{"path": "missing-b"})}},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxIterations = 2
				res, _, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitCap, "hit iteration cap")
			})
		}
	})

	t.Run("token budget", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"ponder"},
			[][]llm.ContentPart{{structuredCall("c1", "read_file", map[string]any{"path": "missing"})}},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxTotalTokens = 15
				res, _, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitBudget, "hit token budget")
			})
		}
	})

	t.Run("dollar budget crossing", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"read_file missing"},
			[][]llm.ContentPart{{structuredCall("c1", "read_file", map[string]any{"path": "missing"})}},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxTotalCostUSD = 0.01
				cfg.CostFn = func(u llm.Usage) (float64, bool) {
					return float64(u.TotalTokens) * 0.001, true
				}
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitBudget, "dollar budget")
				if !strings.Contains(res.Reason, "$0.015") || !strings.Contains(res.Reason, "$0.010") {
					t.Fatalf("Reason = %q, want dollars spent and cap", res.Reason)
				}
				if got := providerCallCount(rec); got != 1 {
					t.Fatalf("provider calls = %d, want 1", got)
				}
			})
		}
	})

	t.Run("dollar budget stops at next turn boundary", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"read_file missing-a", "read_file missing-b", "read_file missing-c"},
			[][]llm.ContentPart{
				{structuredCall("c1", "read_file", map[string]any{"path": "missing-a"})},
				{structuredCall("c2", "read_file", map[string]any{"path": "missing-b"})},
				{structuredCall("c3", "read_file", map[string]any{"path": "missing-c"})},
			},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxIterations = 5
				cfg.MaxTotalCostUSD = 0.02
				cfg.CostFn = func(u llm.Usage) (float64, bool) {
					return float64(u.TotalTokens) * 0.001, true
				}
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitBudget, "dollar budget")
				if got := providerCallCount(rec); got != 2 {
					t.Fatalf("provider calls = %d, want 2 (crossing turn is paid, next is not)", got)
				}
			})
		}
	})

	t.Run("dollar budget disabled", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"read_file missing-a", "read_file missing-b"},
			[][]llm.ContentPart{
				{structuredCall("c1", "read_file", map[string]any{"path": "missing-a"})},
				{structuredCall("c2", "read_file", map[string]any{"path": "missing-b"})},
			},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxIterations = 2
				cfg.MaxTotalCostUSD = 0
				cfg.CostFn = func(llm.Usage) (float64, bool) {
					return 1000, true
				}
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitCap, "hit iteration cap")
				if got := providerCallCount(rec); got != 2 {
					t.Fatalf("provider calls = %d, want 2", got)
				}
			})
		}
	})

	t.Run("dollar budget without pricer continues", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"read_file missing-a", "read_file missing-b"},
			[][]llm.ContentPart{
				{structuredCall("c1", "read_file", map[string]any{"path": "missing-a"})},
				{structuredCall("c2", "read_file", map[string]any{"path": "missing-b"})},
			},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxIterations = 2
				cfg.MaxTotalCostUSD = 0.01
				obs := &recordObserver{}
				cfg.AllowUnpricedSpend = true
				cfg.Obs = obs
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitCap, "hit iteration cap")
				if got := providerCallCount(rec); got != 2 {
					t.Fatalf("provider calls = %d, want 2", got)
				}
				var notes int
				for _, n := range obs.notes {
					if strings.Contains(n, "dollar budget") && strings.Contains(n, "no CostFn") {
						notes++
					}
				}
				if notes != 1 {
					t.Fatalf("missing-pricer notes = %d, want 1; notes=%v", notes, obs.notes)
				}
			})
		}
	})

	t.Run("dollar budget unpriceable usage continues", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"read_file missing-a", "read_file missing-b"},
			[][]llm.ContentPart{
				{structuredCall("c1", "read_file", map[string]any{"path": "missing-a"})},
				{structuredCall("c2", "read_file", map[string]any{"path": "missing-b"})},
			},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxIterations = 2
				cfg.MaxTotalCostUSD = 0.01
				cfg.CostFn = func(llm.Usage) (float64, bool) { return 0, false }
				cfg.AllowUnpricedSpend = true
				obs := &recordObserver{}
				cfg.Obs = obs
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitCap, "hit iteration cap")
				if got := providerCallCount(rec); got != 2 {
					t.Fatalf("provider calls = %d, want 2", got)
				}
				var notes int
				for _, n := range obs.notes {
					if strings.Contains(n, "dollar budget") && strings.Contains(n, "could not be priced") {
						notes++
					}
				}
				if notes != 1 {
					t.Fatalf("unpriceable notes = %d, want 1; notes=%v", notes, obs.notes)
				}
			})
		}
	})
	t.Run("dollar budget unpriceable fails closed", func(t *testing.T) {
		for _, loop := range conformanceLoops(
			[]string{"read_file missing-a", "read_file missing-b"},
			[][]llm.ContentPart{
				{structuredCall("c1", "read_file", map[string]any{"path": "missing-a"})},
				{structuredCall("c2", "read_file", map[string]any{"path": "missing-b"})},
			},
		) {
			t.Run(loop.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.MaxIterations = 2
				cfg.MaxTotalCostUSD = 0.01
				cfg.CostFn = func(llm.Usage) (float64, bool) { return 0, false }
				res, rec, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitBudget, "spend is unpriceable; failing closed")
				if got := providerCallCount(rec); got != 1 {
					t.Fatalf("provider calls = %d, want 1", got)
				}
			})
		}
	})

	t.Run("wall clock", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			run  func(context.Context, Config) (*RunResult, error)
		}{{"Run", Run}, {"RunNative", RunNative}} {
			t.Run(tc.name, func(t *testing.T) {
				cfg := conformanceBaseConfig(t)
				cfg.Model = &blockProvider{}
				cfg.MaxWallClock = 20 * time.Millisecond
				res, err := tc.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, HitDeadline, "hit wall-clock budget")
			})
		}
	})
}

func TestConformance_AbortOnRedBaselineBeforeModel(t *testing.T) {
	for _, loop := range conformanceLoops([]string{"answer should not run"}, [][]llm.ContentPart{{llm.Text("should not run")}}) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			cfg.VerifyCmd = "false"
			cfg.AbortOnRedBaseline = true
			res, rec, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "refused to run")
			if res.Iterations != 0 {
				t.Fatalf("Iterations = %d, want 0", res.Iterations)
			}
			if got := providerCallCount(rec); got != 0 {
				t.Fatalf("provider calls = %d, want 0", got)
			}
		})
	}
}

func TestConformance_CallerCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, Config) (*RunResult, error)
	}{{"Run", Run}, {"RunNative", RunNative}} {
		t.Run(tc.name, func(t *testing.T) {
			bp := &blockProvider{blocked: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := make(chan struct {
				res *RunResult
				err error
			}, 1)
			go func() {
				cfg := conformanceBaseConfig(t)
				cfg.Model = bp
				res, err := tc.run(ctx, cfg)
				out <- struct {
					res *RunResult
					err error
				}{res, err}
			}()
			select {
			case <-bp.blocked:
			case <-time.After(time.Second):
				t.Fatal("provider never entered Generate")
			}
			cancel()
			got := <-out
			if got.err != nil {
				t.Fatal(got.err)
			}
			assertOutcome(t, got.res, Canceled, "canceled")
		})
	}
}

func TestConformance_NativeFinishToolAnswered(t *testing.T) {
	cfg := conformanceBaseConfig(t)
	cfg.Tools = finishTools(t)
	cfg.FinishTool = "say"
	cfg.Model = &nativeScript{turns: [][]llm.ContentPart{{finishCall("f1", map[string]any{"message": "done"})}}}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertOutcome(t, res, Answered, "")
	if res.Answer != "done" {
		t.Fatalf("Answer = %q, want done", res.Answer)
	}
}

func TestConformance_EmptyFinishToolUnverified(t *testing.T) {
	// Fixed by the Slice-1 extraction: an explicit FinishTool call with no message
	// and no bridge arg now flows through the shared empty-answer guard.
	cfg := conformanceBaseConfig(t)
	cfg.Tools = finishTools(t)
	cfg.FinishTool = "say"
	cfg.Model = &nativeScript{turns: [][]llm.ContentPart{{finishCall("f1", map[string]any{})}}}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertOutcome(t, res, Unverified, "empty final answer")
	if res.Answer != "" {
		t.Fatalf("Answer = %q, want empty", res.Answer)
	}
}

func TestConformance_TrustedFinishBypassesCompletionOnly(t *testing.T) {
	// Fixed by the Slice-1 extraction: FinishToolTrustsCaller may vouch for task
	// completion (VerifyCmd/VerifyLastRun and review), but not for safety boundaries,
	// caller cancellation, or the empty-answer guard.
	t.Run("bypasses red VerifyCmd", func(t *testing.T) {
		cfg := conformanceBaseConfig(t)
		cfg.Tools = finishTools(t)
		cfg.FinishTool = "say"
		cfg.FinishToolTrustsCaller = true
		cfg.VerifyCmd = "false"
		cfg.Model = &nativeScript{turns: [][]llm.ContentPart{{finishCall("f1", map[string]any{"message": "done"})}}}
		res, err := RunNative(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertOutcome(t, res, Answered, "")
		if res.Answer != "done" {
			t.Fatalf("Answer = %q, want done", res.Answer)
		}
	})

	t.Run("honors caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cfg := conformanceBaseConfig(t)
		cfg.Tools = finishTools(t)
		cfg.FinishTool = "say"
		cfg.FinishToolTrustsCaller = true
		cfg.Model = &cancelAfterGenerate{cancel: cancel, parts: []llm.ContentPart{finishCall("f1", map[string]any{"message": "done"})}}
		res, err := RunNative(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertOutcome(t, res, Canceled, "canceled")
	})

	t.Run("honors test fence", func(t *testing.T) {
		sb := sbWith(t, map[string]string{"x_test.go": "original\n"})
		cfg := conformanceBaseConfig(t)
		cfg.Sandbox = sb
		cfg.Tools = DefaultTools(sb, time.Second)
		cfg.Tools["say"] = Tool{
			Name:       "say",
			Desc:       "say the final message",
			NativeDesc: "send the final message",
			Run: func(context.Context, string) (string, error) {
				return "", nil
			},
		}
		cfg.FinishTool = "say"
		cfg.FinishToolTrustsCaller = true
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

type cancelAfterGenerate struct {
	cancel context.CancelFunc
	parts  []llm.ContentPart
	text   string
	calls  int
}

func (p *cancelAfterGenerate) Name() string { return "cancel-after-generate" }
func (p *cancelAfterGenerate) Capabilities() llm.Capabilities {
	return llm.Capabilities{Tools: len(p.parts) > 0}
}
func (p *cancelAfterGenerate) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("cancel-after-generate")
}
func (p *cancelAfterGenerate) Generate(context.Context, llm.Request) (*llm.Response, error) {
	p.calls++
	p.cancel()
	if p.parts != nil {
		return &llm.Response{Content: p.parts, FinishReason: llm.FinishStop, Usage: llm.Usage{TotalTokens: 15}}, nil
	}
	return &llm.Response{Content: []llm.ContentPart{llm.Text(p.text)}, FinishReason: llm.FinishStop, Usage: llm.Usage{TotalTokens: 15}}, nil
}

func TestConformance_PrecedenceCancelBeatsVerifyAtAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []llm.ContentPart
		text  string
		run   func(context.Context, Config) (*RunResult, error)
	}{
		{name: "Run", text: "answer done", run: Run},
		{name: "RunNative", parts: []llm.ContentPart{llm.Text("done")}, run: RunNative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cfg := conformanceBaseConfig(t)
			cfg.Model = &cancelAfterGenerate{cancel: cancel, text: tc.text, parts: tc.parts}
			cfg.VerifyCmd = "true"
			res, err := tc.run(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Canceled, "canceled")
		})
	}
}

func TestConformance_PrecedenceVerifyFailSkipsReviewer(t *testing.T) {
	for _, loop := range conformanceLoops([]string{"answer done"}, [][]llm.ContentPart{{llm.Text("done")}}) {
		t.Run(loop.name, func(t *testing.T) {
			rv := &fakeReviewer{verdicts: [][]ReviewFinding{{blocker("x.go", "x")}}}
			cfg := conformanceBaseConfig(t)
			cfg.VerifyCmd = "false"
			cfg.Reviewer = rv
			cfg.ReviewPolicy = ReviewPolicyOptional
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "did not pass")
			if len(rv.inputs) != 0 {
				t.Fatalf("reviewer calls = %d, want 0", len(rv.inputs))
			}
		})
	}
}

func TestConformance_PrecedenceEmptyAnswerAfterGreenVerify(t *testing.T) {
	for _, loop := range conformanceLoops([]string{"answer"}, [][]llm.ContentPart{{}}) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			cfg.VerifyCmd = "true"
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, Unverified, "empty final answer")
		})
	}
}

type countingMem struct {
	mu   sync.Mutex
	adds int
}

func (m *countingMem) Add(context.Context, []mneme.Message, mneme.Scope) ([]mneme.Fact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adds++
	return []mneme.Fact{{Text: "stored"}}, nil
}
func (m *countingMem) Search(context.Context, string, mneme.Scope, int) ([]mneme.Fact, error) {
	return nil, nil
}
func (m *countingMem) Get(context.Context, string) (mneme.Fact, error) { return mneme.Fact{}, nil }
func (m *countingMem) Delete(context.Context, string) error            { return nil }
func (m *countingMem) Close() error                                    { return nil }
func (m *countingMem) count() int                                      { m.mu.Lock(); defer m.mu.Unlock(); return m.adds }

func TestConformance_GroundedMemoryPolicy(t *testing.T) {
	cases := []struct {
		name     string
		text     []string
		native   [][]llm.ContentPart
		wantAdds int
	}{
		{"ungrounded answer", []string{"answer done"}, [][]llm.ContentPart{{llm.Text("done")}}, 0},
		{"grounded answer", []string{"list_dir .", "answer done"}, [][]llm.ContentPart{{structuredCall("c1", "list_dir", map[string]any{"path": "."})}, {llm.Text("done")}}, 1},
	}

	for _, tc := range cases {
		for _, loop := range conformanceLoops(tc.text, tc.native) {
			t.Run(tc.name+"/"+loop.name, func(t *testing.T) {
				mem := &countingMem{}
				cfg := conformanceBaseConfig(t)
				cfg.Memory = mem
				res, _, err := loop.run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				assertOutcome(t, res, Answered, "")
				res.AwaitMemory()
				if got := mem.count(); got != tc.wantAdds {
					t.Fatalf("memory Add calls = %d, want %d", got, tc.wantAdds)
				}
			})
		}
	}
}

func TestConformance_KilledRepeat(t *testing.T) {
	// maxRepeats=2 kills on the 3rd occurrence.
	text := []string{"read_file x", "read_file x", "read_file x"}
	call := structuredCall("c1", "read_file", map[string]any{"path": "x"})
	native := [][]llm.ContentPart{{call}, {call}, {call}}

	for _, loop := range conformanceLoops(text, native) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, KilledRepeat, "repeated")
		})
	}
}

func TestConformance_KilledRepeatIgnoresReasoningWhenObservationStagnates(t *testing.T) {
	// A moving reasoning trace can defer action-only repeat kills, but it is not
	// progress when the world returns the exact same observation for the exact same
	// tool call. The hard observation-keyed ceiling is maxReasoningRepeats
	// occurrences, so a maxIter at that ceiling must kill instead of hitting cap.
	textModel := &scriptedRz{replies: []string{"read_file x"}, advancing: true}
	nativeTurns := make([][]llm.ContentPart, maxReasoningRepeats)
	for i := range nativeTurns {
		nativeTurns[i] = []llm.ContentPart{
			structuredCall("r", "read_file", map[string]any{"path": "x"}),
			llm.ReasoningPart{Raw: []byte(`"thought-` + strings.Repeat("x", i+1) + `"`)},
		}
	}

	for _, tc := range []struct {
		name string
		run  func(context.Context, Config) (*RunResult, error)
	}{
		{name: "Run", run: func(ctx context.Context, cfg Config) (*RunResult, error) {
			cfg.Model = textModel
			return Run(ctx, cfg)
		}},
		{name: "RunNative", run: func(ctx context.Context, cfg Config) (*RunResult, error) {
			cfg.Model = &nativeScript{turns: nativeTurns}
			return RunNative(ctx, cfg)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			cfg.Sandbox = sbWith(t, map[string]string{"x": "same observation\n"})
			cfg.MaxIterations = maxReasoningRepeats
			res, err := tc.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, KilledRepeat, "repeated")
			if res.Iterations > maxReasoningRepeats {
				t.Fatalf("Iterations = %d, want <= %d", res.Iterations, maxReasoningRepeats)
			}
		})
	}
}

func TestConformance_KilledSpiral(t *testing.T) {
	// noProgressWindow=4 discovery calls with DIFFERENT args.
	text := []string{"list_dir a", "list_dir b", "list_dir c", "list_dir d"}
	native := [][]llm.ContentPart{
		{structuredCall("c1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("c2", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("c3", "list_dir", map[string]any{"path": "c"})},
		{structuredCall("c4", "list_dir", map[string]any{"path": "d"})},
	}

	for _, loop := range conformanceLoops(text, native) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, KilledSpiral, "discovery")
		})
	}
}

func TestConformance_KilledStagnant(t *testing.T) {
	// maxStagnant=3 identical failing run fingerprints.
	// Interleave with read_file to avoid KilledRepeat.
	text := []string{
		"run exit 1", "read_file a",
		"run exit 1", "read_file b",
		"run exit 1",
	}
	native := [][]llm.ContentPart{
		{structuredCall("r1", "run", map[string]any{"command": "exit 1"})},
		{structuredCall("a", "read_file", map[string]any{"path": "a"})},
		{structuredCall("r2", "run", map[string]any{"command": "exit 1"})},
		{structuredCall("b", "read_file", map[string]any{"path": "b"})},
		{structuredCall("r3", "run", map[string]any{"command": "exit 1"})},
	}

	for _, loop := range conformanceLoops(text, native) {
		t.Run(loop.name, func(t *testing.T) {
			cfg := conformanceBaseConfig(t)
			cfg.Sandbox = sbWith(t, map[string]string{"a": "1", "b": "2"})
			res, _, err := loop.run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertOutcome(t, res, KilledStagnant, "stuck")
		})
	}
}

var errConformanceCanceled = errors.New("interrupt signal received")

func TestConformance_CallerCancelWithCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, Config) (*RunResult, error)
	}{{"Run", Run}, {"RunNative", RunNative}} {
		t.Run(tc.name, func(t *testing.T) {
			bp := &blockProvider{blocked: make(chan struct{})}
			ctx, cancel := context.WithCancelCause(context.Background())
			out := make(chan struct {
				res *RunResult
				err error
			}, 1)
			go func() {
				cfg := conformanceBaseConfig(t)
				cfg.Model = bp
				res, err := tc.run(ctx, cfg)
				out <- struct {
					res *RunResult
					err error
				}{res, err}
			}()
			select {
			case <-bp.blocked:
			case <-time.After(time.Second):
				t.Fatal("provider never entered Generate")
			}
			cancel(errConformanceCanceled)
			got := <-out
			if got.err != nil {
				t.Fatal(got.err)
			}
			assertOutcome(t, got.res, Canceled, "canceled")
		})
	}
}
