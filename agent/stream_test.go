package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// streamProvider is a deterministic STREAMING fake: the i-th call replays the
// i-th turn's chunks (clamping to the last). It advertises Streaming so the loop
// takes the Stream path, and records whether Generate was called so a test can
// prove the stream path — not Generate — was used when Config.Stream is set.
type streamProvider struct {
	turns     [][]llm.Chunk
	n         int
	genCalled bool
}

func (s *streamProvider) Name() string { return "stream" }
func (s *streamProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{Streaming: true, Tools: true}
}

func (s *streamProvider) Generate(context.Context, llm.Request) (*llm.Response, error) {
	s.genCalled = true
	return &llm.Response{Content: []llm.ContentPart{llm.Text("generate-not-stream")}}, nil
}

func (s *streamProvider) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	i := s.n
	if i >= len(s.turns) {
		i = len(s.turns) - 1
	}
	s.n++
	chunks := s.turns[i]
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
}

// deltaCapture is an Observer that ALSO implements DeltaObserver, recording every
// streamed fragment. Embedding nopObserver supplies the rest of the interface.
type deltaCapture struct {
	nopObserver
	deltas []string
}

func (d *deltaCapture) ModelDelta(s string) { d.deltas = append(d.deltas, s) }

// collectStream collapses a chunk stream into a Response and taps each text delta.
func TestCollectStream(t *testing.T) {
	seq := func(yield func(llm.Chunk, error) bool) {
		_ = yield(llm.Chunk{Text: "hel"}, nil) &&
			yield(llm.Chunk{Text: "lo"}, nil) &&
			yield(llm.Chunk{ToolCall: &llm.ToolCallPart{ID: "1", Name: "read_file", Args: []byte(`{"arg":"x"}`)}}, nil) &&
			yield(llm.Chunk{Done: true, FinishReason: llm.FinishToolUse, Usage: llm.Usage{TotalTokens: 9}}, nil)
	}
	var got []string
	resp, err := collectStream(seq, func(s string) { got = append(got, s) })
	if err != nil {
		t.Fatalf("collectStream: %v", err)
	}
	if resp.Text() != "hello" {
		t.Errorf("text = %q, want %q", resp.Text(), "hello")
	}
	if strings.Join(got, "|") != "hel|lo" {
		t.Errorf("deltas = %v, want [hel lo]", got)
	}
	if resp.FinishReason != llm.FinishToolUse {
		t.Errorf("finish = %q, want tool_use", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 9 {
		t.Errorf("usage carried from Done chunk = %d, want 9", resp.Usage.TotalTokens)
	}
	// Content order: the accumulated text part first, then the tool call.
	if len(resp.Content) != 2 {
		t.Fatalf("content parts = %d, want 2 (text + call)", len(resp.Content))
	}
	if _, ok := resp.Content[1].(llm.ToolCallPart); !ok {
		t.Errorf("second content part = %T, want ToolCallPart", resp.Content[1])
	}
}

// A streamed error aborts collection and propagates (so the eviction wrapper can
// classify ErrContextLength).
func TestCollectStreamError(t *testing.T) {
	seq := func(yield func(llm.Chunk, error) bool) {
		_ = yield(llm.Chunk{Text: "partial"}, nil) && yield(llm.Chunk{}, llm.ErrContextLength)
	}
	if _, err := collectStream(seq, nil); err == nil {
		t.Fatal("expected the streamed error to propagate")
	}
}

// End-to-end: with Config.Stream set and a streaming provider, the loop streams —
// deltas reach a DeltaObserver, the collapsed reply still drives termination, and
// Generate is never called.
func TestRunStreamsDeltasToObserver(t *testing.T) {
	sp := &streamProvider{turns: [][]llm.Chunk{
		{{Text: "answer "}, {Text: "hello world"}, {Done: true, FinishReason: llm.FinishStop}},
	}}
	cap := &deltaCapture{}
	res, err := Run(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Task:    "hi",
		Stream:  true,
		Obs:     cap,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != Answered || res.Answer != "hello world" {
		t.Fatalf("outcome=%q answer=%q, want Answered/\"hello world\"", res.Outcome, res.Answer)
	}
	if strings.Join(cap.deltas, "") != "answer hello world" {
		t.Errorf("streamed deltas = %q, want %q", strings.Join(cap.deltas, ""), "answer hello world")
	}
	if sp.genCalled {
		t.Error("Generate was called despite Stream=true on a streaming provider")
	}
}

// A non-streaming provider ignores Config.Stream and uses Generate — no panic, no
// deltas, behavior unchanged. Guards the "silently fall back" promise.
func TestRunStreamFallsBackWhenUnsupported(t *testing.T) {
	sp := &scripted{replies: []string{"answer ok"}} // Capabilities{} => Streaming false.
	cap := &deltaCapture{}
	res, err := Run(context.Background(), Config{
		Model:   sp,
		Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Task:    "hi",
		Stream:  true,
		Obs:     cap,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "ok" {
		t.Errorf("answer = %q, want ok", res.Answer)
	}
	if len(cap.deltas) != 0 {
		t.Errorf("got %d deltas from a non-streaming provider, want 0", len(cap.deltas))
	}
}
