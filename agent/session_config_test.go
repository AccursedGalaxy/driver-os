package agent

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sort"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// swapProvider is the minimal llm.Provider a SetModel test needs — identity
// only; the stub loop below never calls it.
type swapProvider struct{ id string }

func (p swapProvider) Name() string                   { return "swap" }
func (p swapProvider) Model() string                  { return p.id }
func (p swapProvider) Capabilities() llm.Capabilities { return llm.Capabilities{Tools: true} }
func (p swapProvider) Generate(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{}, nil
}
func (p swapProvider) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(func(llm.Chunk, error) bool) {}
}

// SetModel must change the provider the NEXT Send runs with while the
// conversation accumulated so far is preserved — the /model contract.
func TestSessionSetModelSwapsProviderKeepsConversation(t *testing.T) {
	var sawModel []string
	loop := func(_ context.Context, cfg Config) (*RunResult, error) {
		sawModel = append(sawModel, cfg.Model.(swapProvider).id)
		// Echo a growing transcript so the session has history to preserve.
		msgs := append(append([]llm.Message{}, cfg.History...),
			llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: cfg.Task}}})
		return &RunResult{Outcome: Answered, Answer: "ok", Messages: msgs}, nil
	}

	s := NewSession(Config{Model: swapProvider{id: "cheap/one"}}, loop)
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	before := len(s.Messages())
	if before == 0 {
		t.Fatal("no history after first turn")
	}

	s.SetModel(swapProvider{id: "flagship/two"})
	if got := s.Model().(swapProvider).id; got != "flagship/two" {
		t.Fatalf("Model() = %q after SetModel", got)
	}
	if _, err := s.Send(context.Background(), "second"); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	if len(sawModel) != 2 || sawModel[0] != "cheap/one" || sawModel[1] != "flagship/two" {
		t.Fatalf("loop saw models %v; want [cheap/one flagship/two]", sawModel)
	}
	if len(s.Messages()) <= before {
		t.Fatalf("conversation lost across SetModel: %d -> %d messages", before, len(s.Messages()))
	}
}

// readTurns scripts n distinct read_file calls (varying line ranges so the
// exact-repeat detector never trips) — a model that keeps working forever,
// which is exactly what exercises the iteration cap.
func readTurns(n int) [][]llm.ContentPart {
	turns := make([][]llm.ContentPart, 0, n)
	for i := 0; i < n; i++ {
		turns = append(turns, []llm.ContentPart{
			structuredCall(fmt.Sprintf("c%d", i), "read_file",
				map[string]any{"path": "go.mod", "from": 1, "to": i + 1}),
		})
	}
	return turns
}

// SetMaxIterations must govern the NEXT Send: the same conversation that just
// hit the old cap runs to the new one on "continue" — the /set max-iters seam
// the TUI stands on after a hit_cap turn.
func TestSessionSetMaxIterations(t *testing.T) {
	ns := &nativeScript{turns: readTurns(12)}
	s := NewSession(Config{
		Model:         ns,
		Sandbox:       sbWith(t, map[string]string{"go.mod": "module x\n"}),
		MaxIterations: 2,
	}, RunNative)

	res, err := s.Send(context.Background(), "survey the tree")
	if err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if res.Outcome != HitCap || res.Iterations != 2 {
		t.Fatalf("turn 1 = %s after %d iterations, want %s after 2", res.Outcome, res.Iterations, HitCap)
	}

	s.SetMaxIterations(4)
	if got := s.MaxIterations(); got != 4 {
		t.Fatalf("MaxIterations() = %d after SetMaxIterations(4)", got)
	}
	res, err = s.Send(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if res.Outcome != HitCap || res.Iterations != 4 {
		t.Fatalf("turn 2 = %s after %d iterations, want %s after 4 (new cap not applied)", res.Outcome, res.Iterations, HitCap)
	}
}

// SetTools must govern the NEXT Send: the /skills picker rebuilds the toolset
// (adding or dropping the `skill` meta-tool) on a live conversation.
func TestSessionSetToolsSwapsToolset(t *testing.T) {
	var saw [][]string
	loop := func(_ context.Context, cfg Config) (*RunResult, error) {
		keys := make([]string, 0, len(cfg.Tools))
		for k := range cfg.Tools {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		saw = append(saw, keys)
		return &RunResult{Outcome: Answered, Answer: "ok"}, nil
	}
	echo := func(string) Tool {
		return Tool{Name: "echo", Run: func(context.Context, string) (string, error) { return "", nil }}
	}

	s := NewSession(Config{
		Model: swapProvider{id: "m"},
		Tools: map[string]Tool{"echo": echo("echo")},
	}, loop)
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}

	s.SetTools(map[string]Tool{"echo": echo("echo"), "skill": echo("skill")})
	if got := len(s.Tools()); got != 2 {
		t.Fatalf("Tools() has %d entries after SetTools, want 2", got)
	}
	if _, err := s.Send(context.Background(), "second"); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	want := [][]string{{"echo"}, {"echo", "skill"}}
	if len(saw) != 2 || !slices.Equal(saw[0], want[0]) || !slices.Equal(saw[1], want[1]) {
		t.Fatalf("loop saw toolsets %v; want %v", saw, want)
	}
}

// /review off must turn review OFF for the next Send. Regression: a launch
// that armed a reviewer promoted ReviewPolicyRequired; disarming only the
// Reviewer left the required policy behind, and Config.Validate then bricked
// every subsequent turn with "required review policy needs a Reviewer".
func TestSessionReviewOffDowngradesRequiredPolicy(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("answer")}}}
	s := NewSession(Config{
		Model:        ns,
		Sandbox:      sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Reviewer:     &errReviewer{},
		ReviewPolicy: ReviewPolicyRequired,
	}, RunNative)

	s.SetReviewer(nil)
	s.SetReviewPolicy(ReviewPolicyDefault)
	if got := s.ReviewPolicy(); got != ReviewPolicyDefault {
		t.Fatalf("ReviewPolicy() = %v after SetReviewPolicy(ReviewPolicyDefault)", got)
	}

	if _, err := s.Send(context.Background(), "just answer"); err != nil {
		t.Fatalf("Send after /review off: %v (review off must not require a reviewer)", err)
	}
}

// SetMaxTokens must reach the provider on the NEXT Send's requests.
func TestSessionSetMaxTokens(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("answer one")},
		{llm.Text("answer two")},
	}}
	s := NewSession(Config{
		Model:     ns,
		Sandbox:   sbWith(t, map[string]string{"go.mod": "module x\n"}),
		MaxTokens: 128,
	}, RunNative)

	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if got := ns.calls[len(ns.calls)-1].MaxTokens; got != 128 {
		t.Fatalf("turn 1 request MaxTokens = %d, want 128", got)
	}

	s.SetMaxTokens(256)
	if got := s.MaxTokens(); got != 256 {
		t.Fatalf("MaxTokens() = %d after SetMaxTokens(256)", got)
	}
	if _, err := s.Send(context.Background(), "second"); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if got := ns.calls[len(ns.calls)-1].MaxTokens; got != 256 {
		t.Fatalf("turn 2 request MaxTokens = %d, want 256 (new cap not applied)", got)
	}
}
