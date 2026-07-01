package agent

import (
	"context"
	"iter"
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
