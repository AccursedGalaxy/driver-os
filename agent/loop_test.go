package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// scripted is a deterministic llm.Provider for testing the LOOP (not the model):
// it returns the i-th canned reply on the i-th Generate call (clamping to the
// last so an over-long run keeps emitting the final line), and RECORDS every
// Request it was handed. Recording is the point — it lets a test assert what
// context the loop actually built (the P1 invariant: state lives in the harness
// and the full conversation is re-sent every turn).
//
// Because llm.Provider is a one-method interface, this whole mock is ~15 lines —
// the leverage that makes the stochastic loop deterministically testable.
type scripted struct {
	replies []string
	err     error         // if non-nil, Generate returns it (after recording the call).
	calls   []llm.Request // every Request received, in order.
}

func (s *scripted) Name() string                   { return "scripted" }
func (s *scripted) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (s *scripted) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("scripted")
}

func (s *scripted) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	i := len(s.calls) - 1
	if i >= len(s.replies) {
		i = len(s.replies) - 1 // clamp: repeat the last scripted reply.
	}
	return &llm.Response{
		Content: []llm.ContentPart{llm.Text(s.replies[i])},
		// Fixed per-call usage so a test can assert the loop SUMS it (free metric,
		// straight from llm.Usage — no estimation).
		Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

// runScript wires a scripted provider into Run over a fresh temp sandbox and
// returns both the result and the mock (so a test can inspect recorded calls).
func runScript(t *testing.T, files map[string]string, replies []string) (*RunResult, *scripted) {
	t.Helper()
	sp := &scripted{replies: replies}
	res, err := Run(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, files),
		Task:    "test task",
	})
	if err != nil {
		t.Fatalf("Run returned an unexpected error: %v", err)
	}
	return res, sp
}

// ---- the five terminal outcomes, each forced deterministically ----

func TestRunAnswered(t *testing.T) {
	// list_dir grounds the run; answer finishes it.
	res, _ := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"list_dir .", "answer it is x"})
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, Answered, res.Reason)
	}
	if res.Answer != "it is x" {
		t.Errorf("Answer = %q, want %q", res.Answer, "it is x")
	}
	if res.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", res.Iterations)
	}
}

// An `answer` verb with no content is the model emitting the done-signal without
// actually answering. It must NOT be recorded as Answered/exit-0 (an empty
// answer once passed as a clean run); the empty-answer guard flags it Unverified.
func TestRunEmptyAnswerIsUnverified(t *testing.T) {
	res, _ := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"answer"})
	if res.Outcome != Unverified {
		t.Fatalf("Outcome = %q (%s), want Unverified for an empty answer", res.Outcome, res.Reason)
	}
	if res.Reason == "" {
		t.Errorf("expected a reason explaining the empty-answer rejection")
	}
}

func TestRunKilledSpiral(t *testing.T) {
	// noProgressWindow (4) list_dir calls in a row with DIFFERENT args — exact
	// repeat never fires, the spiral detector does. This guards the fix-3 gating.
	res, _ := runScript(t, nil,
		[]string{"list_dir a", "list_dir b", "list_dir c", "list_dir d"})
	if res.Outcome != KilledSpiral {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, KilledSpiral, res.Reason)
	}
	if res.Iterations != noProgressWindow {
		t.Errorf("Iterations = %d, want %d", res.Iterations, noProgressWindow)
	}
}

func TestRunKilledRepeat(t *testing.T) {
	// The SAME action repeated trips the exact-repeat detector (repeats >=
	// maxRepeats). read_file (not list_dir) so the spiral detector stays out of it.
	res, _ := runScript(t, nil,
		[]string{"read_file x", "read_file x", "read_file x"})
	if res.Outcome != KilledRepeat {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, KilledRepeat, res.Reason)
	}
}

// scriptedRz is a scripted provider that also attaches an opaque reasoning trace
// to each turn. advancing=true makes the trace differ every call (a thinking model
// still moving its chain of thought); advancing=false freezes it (a stalled trace);
// nonceTrace=true keeps the trace differing but reports zero reasoning tokens (the
// gemini-via-OpenRouter encrypted thought-signature, DUET-DOGFOOD N3). It drives
// the two-threshold tight-loop detector (maxRepeats vs maxReasoningRepeats).
type scriptedRz struct {
	replies    []string
	advancing  bool
	nonceTrace bool
	calls      int
}

func (s *scriptedRz) Name() string                   { return "scripted-rz" }
func (s *scriptedRz) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (s *scriptedRz) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("scripted-rz")
}

func (s *scriptedRz) Generate(_ context.Context, _ llm.Request) (*llm.Response, error) {
	i := s.calls
	s.calls++
	if i >= len(s.replies) {
		i = len(s.replies) - 1
	}
	raw := `"frozen-thought"`
	if s.advancing || s.nonceTrace {
		raw = `"thought-` + strings.Repeat("x", s.calls) + `"` // differs every call.
	}
	rz := 3
	if s.nonceTrace {
		rz = 0 // trace moves, but no reasoning was spent — a per-request nonce.
	}
	return &llm.Response{
		Content: []llm.ContentPart{llm.Text(s.replies[i]), llm.ReasoningPart{Raw: []byte(raw)}},
		Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, ReasoningTokens: rz},
	}, nil
}

func TestRunReasoningAdvancedUsesLenientThreshold(t *testing.T) {
	// A thinking model re-issues the SAME action while its reasoning trace advances.
	// The strict maxRepeats=2 would kill at iter 3; the lenient maxReasoningRepeats=6
	// must let it run to the cap instead. read_file (not list_dir) to keep the spiral
	// detector out of it.
	sp := &scriptedRz{replies: []string{"read_file x"}, advancing: true}
	res, err := Run(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, nil), Task: "test task", MaxIterations: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == KilledRepeat {
		t.Fatalf("Outcome = KilledRepeat — advancing reasoning should defer to maxReasoningRepeats, not strict maxRepeats")
	}
	if res.Outcome != HitCap {
		t.Fatalf("Outcome = %q, want %q (ran to cap under the lenient threshold)", res.Outcome, HitCap)
	}
	for _, s := range res.Steps[1:] { // turn 1 has no prior trace to compare.
		if !s.ReasoningAdvanced {
			t.Errorf("step %d ReasoningAdvanced = false, want true", s.Iter)
		}
	}
}

func TestRunZeroTokenMovingTraceKeepsLenientThreshold(t *testing.T) {
	// Text-loop twin of the native zero-token test: a moving trace with ZERO
	// reported reasoning tokens still counts as advancing (gemini's encrypted
	// thought-signature shape — a token gate was tried and reverted 2026-06-12
	// after it false-killed real work; see loop_tools.go). MaxIterations sits
	// below the lenient ceiling, so surviving to the cap proves the strict
	// threshold did not fire.
	sp := &scriptedRz{replies: []string{"read_file x"}, nonceTrace: true}
	res, err := Run(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, nil), Task: "test task", MaxIterations: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == KilledRepeat {
		t.Fatalf("Outcome = KilledRepeat (%s) — a moving zero-token trace must keep the lenient threshold", res.Reason)
	}
	if res.Outcome != HitCap {
		t.Fatalf("Outcome = %q, want %q (ran to cap under the lenient threshold)", res.Outcome, HitCap)
	}
}

func TestRunReasoningFrozenUsesStrictThreshold(t *testing.T) {
	// A frozen reasoning trace (unchanged bytes every turn) is a real stall: the
	// detector must fall back to the strict maxRepeats and kill, exactly as it does
	// for a non-thinking model.
	sp := &scriptedRz{replies: []string{"read_file x"}, advancing: false}
	res, err := Run(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, nil), Task: "test task", MaxIterations: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledRepeat {
		t.Fatalf("Outcome = %q, want %q (frozen trace -> strict threshold)", res.Outcome, KilledRepeat)
	}
}

func TestRunCapturesStepTiming(t *testing.T) {
	// Every step records a model-call duration; a tool turn also records a tool
	// duration. The answer turn dispatches no tool, so its ToolMs stays 0.
	res, _ := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"list_dir .", "answer done"})
	if len(res.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(res.Steps))
	}
	if res.Steps[0].ToolMs < 0 || res.Steps[0].ModelMs < 0 {
		t.Errorf("negative timing on tool turn: %+v", res.Steps[0])
	}
	if res.Steps[1].ToolMs != 0 {
		t.Errorf("answer turn ToolMs = %d, want 0 (no tool dispatched)", res.Steps[1].ToolMs)
	}
}

func TestRunRespectsConfigMaxIterations(t *testing.T) {
	// A model that never answers, with distinct non-list_dir actions so neither
	// no-progress detector fires — OUR configured cap (3), not DefaultMaxIterations,
	// must be what stops it (P5/P7: the termination knob is the caller's).
	sp := &scripted{replies: []string{"read_file a", "read_file b", "read_file c", "read_file d", "read_file e"}}
	res, err := Run(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, nil),
		Task:          "test task",
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != HitCap {
		t.Fatalf("Outcome = %q, want %q", res.Outcome, HitCap)
	}
	if res.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3 (the configured cap, not DefaultMaxIterations=%d)", res.Iterations, DefaultMaxIterations)
	}
}

func TestRunHitCap(t *testing.T) {
	// DefaultMaxIterations distinct read_file calls: no exact repeat, not list_dir, so
	// neither no-progress detector fires — the hard cap is the only backstop (P5).
	replies := make([]string, DefaultMaxIterations)
	for i := range replies {
		replies[i] = "read_file f" + string(rune('0'+i))
	}
	res, _ := runScript(t, nil, replies)
	if res.Outcome != HitCap {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, HitCap, res.Reason)
	}
	if res.Iterations != DefaultMaxIterations {
		t.Errorf("Iterations = %d, want %d", res.Iterations, DefaultMaxIterations)
	}
}

func TestRunVerifyCmdFailMarksUnverified(t *testing.T) {
	// Text-loop counterpart of the native gate: grounded write + an `answer`, but the
	// caller's verification command fails -> Unverified, not Answered.
	sp := &scripted{replies: []string{"write_file ok.txt hi", "answer done"}}
	res, err := Run(context.Background(), Config{
		Model: sp, Sandbox: sbWith(t, nil), Task: "t", VerifyCmd: "exit 1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Unverified {
		t.Fatalf("Outcome = %q (%s), want Unverified", res.Outcome, res.Reason)
	}
	if res.Answer != "done" {
		t.Errorf("Answer = %q, want the answer preserved on an unverified finish", res.Answer)
	}
}

func TestRunStagnantObservationKilled(t *testing.T) {
	// Text-loop A2: a failing `run` whose identical result recurs across turns with
	// DIFFERENT actions between (read_file) — exact-repeat never fires, spiral never
	// fires — is ended by the stagnant-observation detector at the 3rd identical fail.
	fail := "run echo boom 1>&2; exit 2"
	res, err := Run(context.Background(), Config{
		Model:         &scripted{replies: []string{fail, "read_file a.txt", fail, "read_file b.txt", fail}},
		Sandbox:       sbWith(t, map[string]string{"a.txt": "x\n", "b.txt": "y\n"}),
		Task:          "t",
		MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledStagnant {
		t.Fatalf("Outcome = %q (%s), want KilledStagnant", res.Outcome, res.Reason)
	}
	if res.Iterations != 5 {
		t.Errorf("Iterations = %d, want 5 (killed on the 3rd identical failure)", res.Iterations)
	}
}

func TestRunProviderError(t *testing.T) {
	boom := errors.New("transport exploded")
	sp := &scripted{err: boom}
	res, err := Run(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, nil),
		Task:    "test task",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	if res.Outcome != ProviderErr {
		t.Fatalf("Outcome = %q, want %q", res.Outcome, ProviderErr)
	}
	if res.Iterations != 0 || len(res.Steps) != 0 {
		t.Errorf("on provider failure want no recorded turns; got Iterations=%d Steps=%d", res.Iterations, len(res.Steps))
	}
}

// ---- the malformed-reply path: a chatty model with no recognized verb ----

func TestRunMalformedReplyRecovers(t *testing.T) {
	// First reply has no recognized verb; the loop must feed back the
	// no-action-recognized observation (P6), not crash, and the model recovers.
	res, _ := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"hmm let me think about this", "list_dir .", "answer done"})
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, Answered, res.Reason)
	}
	first := res.Steps[0]
	if first.Verb != "" {
		t.Errorf("malformed reply parsed Verb = %q, want empty", first.Verb)
	}
	if !strings.HasPrefix(first.Observation, "ERROR: no action recognized") {
		t.Errorf("malformed reply observation = %q, want a no-action-recognized error", first.Observation)
	}
}

// ---- the P1 context invariant: full state re-sent, growing every turn ----

func TestRunContextGrowsEveryTurn(t *testing.T) {
	// Two grounding turns then an answer. The recorded Requests must show the
	// conversation accumulating: each turn re-sends everything plus the prior
	// turn's assistant reply AND its observation.
	_, sp := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"list_dir .", "list_dir .", "answer done"})

	if len(sp.calls) != 3 {
		t.Fatalf("recorded %d Generate calls, want 3", len(sp.calls))
	}

	// Message counts: turn 1 sees just TASK; each later turn adds the previous
	// assistant reply + its observation (2 messages).
	want := []int{1, 3, 5}
	for i, w := range want {
		if got := len(sp.calls[i].Messages); got != w {
			t.Errorf("turn %d carried %d messages, want %d", i+1, got, w)
		}
	}

	// By turn 3 the context must literally contain BOTH prior observations — the
	// anchor against the model flying blind on its own narrative (P4). A loop bug
	// that dropped observations would still "work" but silently fail this.
	obs := 0
	for _, m := range sp.calls[2].Messages {
		if strings.HasPrefix(m.Text(), "OBSERVATION:") {
			obs++
		}
	}
	if obs != 2 {
		t.Errorf("turn 3 context held %d observations, want 2", obs)
	}

	// The system prompt (protocol + tool reference) rides on every call and is
	// stable — it is state we own, re-sent verbatim (P1, P7).
	for i, c := range sp.calls {
		if !strings.Contains(c.System, "EXACTLY ONE line") {
			t.Errorf("turn %d lost the system prompt", i+1)
		}
	}
}

// ---- the free metric: per-turn Usage summed into the run total ----

func TestRunAccumulatesUsage(t *testing.T) {
	res, _ := runScript(t, map[string]string{"go.mod": "module x\n"},
		[]string{"list_dir .", "answer done"})
	// 2 turns * TotalTokens 15 each.
	if res.Usage.TotalTokens != 30 {
		t.Errorf("summed TotalTokens = %d, want 30", res.Usage.TotalTokens)
	}
	for _, s := range res.Steps {
		if s.Usage.TotalTokens != 15 {
			t.Errorf("step %d Usage.TotalTokens = %d, want 15 (per-turn usage lost)", s.Iter, s.Usage.TotalTokens)
		}
	}
}

// ---- cancel outcome tests ----

// blockProvider blocks in Generate until ctx is done, then returns ctx.Err().
// It signals when it entered Generate via the blocked channel (if non-nil).
type blockProvider struct {
	blocked chan struct{}
}

func (p *blockProvider) Name() string                   { return "block" }
func (p *blockProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *blockProvider) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("block")
}
func (p *blockProvider) Generate(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	if p.blocked != nil {
		close(p.blocked)
		p.blocked = nil
	}
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

func TestRunCanceledMidCall(t *testing.T) {
	bp := &blockProvider{blocked: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())

	type runOut struct {
		res *RunResult
		err error
	}
	ch := make(chan runOut, 1)
	go func() {
		res, err := Run(ctx, Config{Model: bp, Sandbox: sbWith(t, nil), Task: "test"})
		ch <- runOut{res, err}
	}()

	// Wait for Generate to be entered, then cancel.
	select {
	case <-bp.blocked:
	case <-time.After(time.Second):
		t.Fatal("provider never entered Generate")
	}
	cancel()

	out := <-ch
	if out.err != nil {
		t.Fatalf("Run returned unexpected err: %v", out.err)
	}
	if out.res.Outcome != Canceled {
		t.Fatalf("Outcome = %q, want Canceled", out.res.Outcome)
	}
	if out.res.ID == "" {
		t.Error("ID should be non-empty on cancel")
	}
	if out.res.Err != nil {
		t.Error("RunResult.Err should be nil for a clean cancel (not a provider fault)")
	}
}

func TestRunCanceledAtAnswerTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the run starts

	sp := &scripted{replies: []string{"answer done"}}
	exec := &countingExecSandbox{}
	res, err := Run(ctx, Config{
		Model:         sp,
		Sandbox:       sbWith(t, nil),
		Task:          "test",
		VerifySandbox: exec,
		VerifyCmd:     "go build ./...",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Outcome != Canceled {
		t.Fatalf("Outcome = %q, want Canceled", res.Outcome)
	}
	if exec.execs != 0 {
		t.Errorf("verify command should NOT have run on canceled ctx; got %d exec(s)", exec.execs)
	}
	if res.Err != nil {
		t.Error("RunResult.Err should be nil for a clean cancel")
	}
}

func TestRunNativeCanceledAtAnswerTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the run starts

	sp := &scripted{replies: []string{"answer done"}}
	exec := &countingExecSandbox{}
	res, err := RunNative(ctx, Config{
		Model:         sp,
		Sandbox:       sbWith(t, nil),
		Task:          "test",
		VerifySandbox: exec,
		VerifyCmd:     "go build ./...",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Outcome != Canceled {
		t.Fatalf("Outcome = %q, want Canceled", res.Outcome)
	}
	if exec.execs != 0 {
		t.Errorf("verify command should NOT have run on canceled ctx; got %d exec(s)", exec.execs)
	}
}

func TestRunMaxWallClockYieldsHitDeadlineNotCanceled(t *testing.T) {
	// A blocking provider that only returns when the ctx is done.
	// MaxWallClock creates a derived loopCtx with a deadline; when it expires,
	// loopCtx.Err() is DeadlineExceeded, but the parent ctx is still alive.
	// The loop must record HitDeadline, NOT Canceled.
	bp := &blockProvider{}
	ctx := context.Background() // parent is never canceled

	shortBudget := 50 * time.Millisecond
	res, err := Run(ctx, Config{
		Model:        bp,
		Sandbox:      sbWith(t, nil),
		Task:         "test",
		MaxWallClock: shortBudget,
	})
	// The wall-clock deadline fires mid-call, which is a typed stop (not an err).
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Outcome != HitDeadline {
		t.Fatalf("Outcome = %q, want HitDeadline (MaxWallClock expiry must not be mislabeled Canceled)", res.Outcome)
	}
	if res.Err != nil {
		t.Error("RunResult.Err should be nil for HitDeadline")
	}
}

func TestRunNativeMaxWallClockYieldsHitDeadlineNotCanceled(t *testing.T) {
	bp := &blockProvider{}
	ctx := context.Background()

	shortBudget := 50 * time.Millisecond
	res, err := RunNative(ctx, Config{
		Model:        bp,
		Sandbox:      sbWith(t, nil),
		Task:         "test",
		MaxWallClock: shortBudget,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Outcome != HitDeadline {
		t.Fatalf("Outcome = %q, want HitDeadline (MaxWallClock expiry must not be mislabeled Canceled)", res.Outcome)
	}
}
