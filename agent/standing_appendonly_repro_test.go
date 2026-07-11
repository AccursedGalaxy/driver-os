package agent

// Repro for the 2026-07-11 P0 cost bug (backlog §"TUI cost session"): the
// standing-context block was sent as an EPHEMERAL trailing user message
// (llm.Request.StandingContext) that changed every iteration. OpenAI-family
// prompt caching via OpenRouter only serves reads when a previously stored
// prompt is an exact prefix of the new request, so the mutating trailer
// zeroed cache reads for entire TUI sessions (measured: 0.0% cached on all
// stream+standing runs, n=80 run reports, vs 85–98% headless; live
// bisection confirmed the trailer is the killer on both gpt-5.6-sol and
// -terra). The fix: the standing block must ride IN the persistent message
// transcript, append-only — never as a side-channel trailer.
//
// These assertions are written against behavior, not the (removed) field, so
// the file compiles before and after the fix. Fenced: the gate, not the
// solver, owns this oracle.

import (
	"context"
	"encoding/json"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// appendOnlyCaptureModel records every request and drives a three-call run:
// two tool-call turns (so the working tree can change mid-run) and an answer.
type appendOnlyCaptureModel struct {
	calls []llm.Request
}

func (m *appendOnlyCaptureModel) Name() string { return "append-only-capture" }
func (m *appendOnlyCaptureModel) Capabilities() llm.Capabilities {
	return llm.Capabilities{Tools: true}
}
func (m *appendOnlyCaptureModel) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("append-only-capture")
}
func (m *appendOnlyCaptureModel) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	m.calls = append(m.calls, req)
	switch len(m.calls) {
	case 1:
		return &llm.Response{Content: []llm.ContentPart{llm.ToolCallPart{ID: "c1", Name: "noop", Args: json.RawMessage(`{}`)}}}, nil
	case 2:
		return &llm.Response{Content: []llm.ContentPart{llm.ToolCallPart{ID: "c2", Name: "noop", Args: json.RawMessage(`{}`)}}}, nil
	}
	return &llm.Response{Content: []llm.ContentPart{llm.Text("done")}}, nil
}

// messageHasStandingBanner reports whether a message carries the standing
// block (any content part containing the banner).
func messageHasStandingBanner(msg llm.Message) bool {
	for _, p := range msg.Parts {
		if tp, ok := p.(llm.TextPart); ok && strings.Contains(tp.Text, "=== STANDING CONTEXT") {
			return true
		}
	}
	return false
}

// TestStandingContextRidesInMessagesAppendOnly is the oracle for the fix:
//
//  1. Every model call in a StandingContext-enabled run must carry the
//     standing block INSIDE req.Messages (as a persisted user message) — not
//     in any side channel the provider would splice in as a mutating trailer.
//  2. The request sequence must be append-only: call i's Messages must be
//     exactly the leading prefix of call i+1's Messages. This is the property
//     OpenAI-family prompt caching requires; any rewrite or relocation of an
//     earlier message forfeits all cache reads.
//  3. Standing messages must persist into the final transcript
//     (RunResult.Messages), so multi-turn Sessions replay them verbatim and
//     stay append-only across turns too.
func TestStandingContextRidesInMessagesAppendOnly(t *testing.T) {
	sb := sbWith(t, nil)
	m := &appendOnlyCaptureModel{}
	res, err := RunNative(context.Background(), Config{
		Model:           m,
		Sandbox:         sb,
		Task:            "do it",
		StandingContext: true,
		Tools: map[string]Tool{"noop": {
			Name:   "noop",
			Schema: json.RawMessage(`{"type":"object","properties":{}}`),
			RunJSON: func(context.Context, json.RawMessage) (string, error) {
				return "observed", nil
			},
		}},
		MaxIterations: 4,
		Obs:           nopObserver{},
	})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if len(m.calls) < 3 {
		t.Fatalf("want >=3 model calls, got %d", len(m.calls))
	}

	// (1) The standing block rides in the persistent messages of every call.
	for i, req := range m.calls {
		found := false
		for _, msg := range req.Messages {
			if msg.Role == llm.RoleUser && messageHasStandingBanner(msg) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("call %d: standing block not present in req.Messages — it must be a persisted user message, not an ephemeral trailer", i+1)
		}
	}

	// (2) Append-only: each call's message list is the exact leading prefix
	// of the next call's.
	for i := 1; i < len(m.calls); i++ {
		prev, cur := m.calls[i-1].Messages, m.calls[i].Messages
		if len(cur) < len(prev) {
			t.Fatalf("call %d shrank the transcript: %d -> %d messages", i+1, len(prev), len(cur))
		}
		if !reflect.DeepEqual(prev, cur[:len(prev)]) {
			t.Errorf("call %d is not an append-only extension of call %d — an earlier message was rewritten or relocated (this forfeits all OpenAI-family cache reads)", i+1, i)
		}
	}

	// (3) The standing message(s) survive into the final transcript.
	found := false
	for _, msg := range res.Messages {
		if msg.Role == llm.RoleUser && messageHasStandingBanner(msg) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RunResult.Messages lacks the standing block — it must persist so Session replays stay append-only across turns")
	}
}
