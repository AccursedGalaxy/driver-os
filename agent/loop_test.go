package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

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
