package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// lastUserText returns the text of the final user message in a request — the turn
// the loop is asking the model to act on. A continuing Send puts the new input
// there, plain (no "TASK:" reframing).
func lastUserText(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return req.Messages[i].Text()
		}
	}
	return ""
}

// containsText reports whether any message in the request carries the substring —
// used to prove a later turn still sees an earlier turn's content (the whole point
// of a Session: state is carried, not rebuilt).
func containsText(req llm.Request, sub string) bool {
	for _, m := range req.Messages {
		if strings.Contains(m.Text(), sub) {
			return true
		}
	}
	return false
}

// A Session's second turn must SEE the first turn: the loop call for turn 2 starts
// from turn 1's full transcript (Config.History) plus the new input as a plain
// user message — not a fresh "TASK:" framing. This is the continuation seam that a
// chat front-end stands on (text loop).
func TestSessionCarriesHistoryTextLoop(t *testing.T) {
	// turn 1: ground then answer; turn 2: answer straight off. The scripted
	// provider is shared across Sends, so its reply index continues: call0/1 serve
	// turn 1, call2 serves turn 2.
	sp := &scripted{replies: []string{"list_dir .", "answer first reply", "answer second reply"}}
	s := NewSession(Config{Model: sp, Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"})}, Run)

	res1, err := s.Send(context.Background(), "what is the module")
	if err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if res1.Outcome != Answered || res1.Answer != "first reply" {
		t.Fatalf("turn 1 = %q/%q, want Answered/\"first reply\"", res1.Outcome, res1.Answer)
	}
	turn1Calls := len(sp.calls)

	res2, err := s.Send(context.Background(), "and the go version")
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if res2.Outcome != Answered || res2.Answer != "second reply" {
		t.Fatalf("turn 2 = %q/%q, want Answered/\"second reply\"", res2.Outcome, res2.Answer)
	}

	// The first call of turn 2 is the one that proves continuity.
	turn2First := sp.calls[turn1Calls]
	if got := lastUserText(turn2First); got != "and the go version" {
		t.Errorf("turn 2 last user message = %q, want the plain new input (no TASK: prefix)", got)
	}
	if !containsText(turn2First, "TASK: what is the module") {
		t.Error("turn 2 does not carry turn 1's original TASK framing — history was lost")
	}
	if !containsText(turn2First, "first reply") {
		t.Error("turn 2 does not carry turn 1's assistant answer — history was lost")
	}
}

// Same continuation guarantee for the native tool-calling loop.
func TestSessionCarriesHistoryNativeLoop(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("answer alpha")}, // turn 1: no tool call => answers
		{llm.Text("answer beta")},  // turn 2
	}}
	s := NewSession(Config{Model: ns, Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"})}, RunNative)

	if _, err := s.Send(context.Background(), "first question"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	turn1Calls := len(ns.calls)
	if _, err := s.Send(context.Background(), "second question"); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	turn2First := ns.calls[turn1Calls]
	if got := lastUserText(turn2First); got != "second question" {
		t.Errorf("turn 2 last user message = %q, want plain new input", got)
	}
	if !containsText(turn2First, "TASK: first question") {
		t.Error("native turn 2 lost turn 1's original framing")
	}
	if !containsText(turn2First, "alpha") {
		t.Error("native turn 2 lost turn 1's assistant reply")
	}
}

// Reset drops the dialogue: the next Send starts fresh ("TASK:" framing, no prior
// turns) while the Session — and its warm sandbox/memory — keeps running.
func TestSessionReset(t *testing.T) {
	sp := &scripted{replies: []string{"answer one", "answer two"}}
	s := NewSession(Config{Model: sp, Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"})}, Run)

	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if len(s.Messages()) == 0 {
		t.Fatal("expected history after a Send")
	}
	s.Reset()
	if len(s.Messages()) != 0 {
		t.Fatalf("Reset left %d messages, want 0", len(s.Messages()))
	}

	callsBefore := len(sp.calls)
	if _, err := s.Send(context.Background(), "fresh start"); err != nil {
		t.Fatalf("Send after Reset: %v", err)
	}
	first := sp.calls[callsBefore]
	// Prefix, not equality: a fresh run may carry the ENVIRONMENT preamble; the
	// contract here is the single-shot TASK framing (and no leaked prior turn).
	if got := lastUserText(first); !strings.HasPrefix(got, "TASK: fresh start") {
		t.Errorf("after Reset the turn = %q, want the fresh single-shot framing %q", got, "TASK: fresh start")
	}
	if containsText(first, "first") {
		t.Error("after Reset the prior turn still leaked into the conversation")
	}
}

// The caller's History slice must not be mutated by a run (the loop clones before
// appending) — otherwise a Session re-using the slice across turns would corrupt
// it. Guards seedMessages's clone.
func TestSeedMessagesDoesNotMutateHistory(t *testing.T) {
	hist := []llm.Message{llm.User("TASK: original"), llm.Assistant("ok")}
	cfg := Config{History: hist, Task: "next"}
	got := seedMessages(cfg, "")

	if len(got) != 3 {
		t.Fatalf("seeded %d messages, want 3 (2 history + 1 input)", len(got))
	}
	if len(hist) != 2 {
		t.Fatalf("history slice grew to %d — it was mutated", len(hist))
	}
	if got[2].Text() != "next" {
		t.Errorf("seeded input = %q, want plain %q", got[2].Text(), "next")
	}
}

// A turn cancelled mid-flight (Ctrl-C) must NOT advance the conversation: the
// partial transcript can end on a tool-call without its result, and committing it
// would malform the next request. The prior clean history must survive so the next
// Send continues from it.
func TestSessionCancelledTurnDoesNotAdvanceHistory(t *testing.T) {
	sp := &scripted{replies: []string{"answer committed"}}
	s := NewSession(Config{Model: sp, Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"})}, Run)

	// First turn completes normally and establishes history.
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	committed := append([]llm.Message(nil), s.Messages()...)
	if len(committed) == 0 {
		t.Fatal("expected history after the first turn")
	}

	// Second turn is cancelled before it runs — history must be unchanged.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Send(ctx, "second"); err == nil {
		t.Log("cancelled Send returned nil error (loop recorded a typed outcome) — still must not advance")
	}
	if got := s.Messages(); len(got) != len(committed) || got[len(got)-1].Text() != committed[len(committed)-1].Text() {
		t.Errorf("cancelled turn advanced history: now %d messages, want the prior %d", len(got), len(committed))
	}
}

// SendParts must attach this turn's images to the first user message the loop
// sends to the model, preserving the existing text framing and ordering images
// after the text part.
func TestSessionSendPartsFlowsImagesToFirstUserMessage(t *testing.T) {
	sp := &scripted{replies: []string{"answer saw it"}}
	s := NewSession(Config{Model: sp}, Run)
	img := llm.ImagePart{MIME: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}

	res, err := s.SendParts(context.Background(), "describe this image", []llm.ImagePart{img})
	if err != nil {
		t.Fatalf("SendParts: %v", err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s, want %s (reason: %s)", res.Outcome, Answered, res.Reason)
	}
	if len(sp.calls) == 0 || len(sp.calls[0].Messages) == 0 {
		t.Fatalf("provider saw no messages: %#v", sp.calls)
	}

	first := sp.calls[0].Messages[0]
	if first.Role != llm.RoleUser {
		t.Fatalf("first message role = %s, want %s", first.Role, llm.RoleUser)
	}
	if len(first.Parts) != 2 {
		t.Fatalf("first user parts = %#v, want text + image", first.Parts)
	}
	text, ok := first.Parts[0].(llm.TextPart)
	if !ok || text.Text != "TASK: describe this image" {
		t.Fatalf("part 0 = %#v, want TASK text", first.Parts[0])
	}
	gotImg, ok := first.Parts[1].(llm.ImagePart)
	if !ok {
		t.Fatalf("part 1 = %#v, want image", first.Parts[1])
	}
	if gotImg.MIME != img.MIME || gotImg.URL != img.URL || !bytes.Equal(gotImg.Data, img.Data) {
		t.Fatalf("image part = %#v, want %#v", gotImg, img)
	}
}

// With no History, seedMessages reproduces the historical single-shot framing.
func TestSeedMessagesSingleShot(t *testing.T) {
	got := seedMessages(Config{Task: "do a thing"}, "")
	if len(got) != 1 || got[0].Text() != "TASK: do a thing" {
		t.Fatalf("single-shot seed = %v, want one %q message", got, "TASK: do a thing")
	}
}

func TestNewSessionWithSeedsNextSendHistory(t *testing.T) {
	seed := []llm.Message{llm.User("TASK: previous"), llm.Assistant("previous answer")}
	var got Config
	loop := func(ctx context.Context, cfg Config) (*RunResult, error) {
		got = cfg
		return &RunResult{Messages: append(append([]llm.Message(nil), cfg.History...), llm.User(cfg.Task), llm.Assistant("next answer"))}, nil
	}
	s := NewSessionWith(Config{}, loop, seed)
	if _, err := s.SendParts(context.Background(), "next", nil); err != nil {
		t.Fatalf("SendParts: %v", err)
	}
	if len(got.History) != 2 || got.History[0].Text() != "TASK: previous" || got.History[1].Text() != "previous answer" {
		t.Fatalf("History = %#v, want seeded two-message history", got.History)
	}
	if got.Task != "next" {
		t.Fatalf("Task = %q, want next", got.Task)
	}
}
