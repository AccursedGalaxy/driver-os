package openaicompat

// Tests for streamed `reasoning_details` reassembly: streaming must surface the
// SAME reasoning trace the non-streaming path attaches (one ReasoningPart with
// the full array), or a streamed turn replays without its trace — the Gemini
// encrypted-signature spiral, this time on the streaming path.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// TestStreamReassemblesReasoningDetails replays the REAL captured deepseek
// stream and checks the Reasoning chunk carries the full reasoning text,
// reassembled from the per-delta fragments, in the non-streaming array shape.
func TestStreamReassemblesReasoningDetails(t *testing.T) {
	raw, err := os.ReadFile(keepaliveFixture)
	if err != nil {
		t.Fatal(err)
	}
	// Ground truth: concatenate every fragment's text straight out of the fixture.
	var want strings.Builder
	re := regexp.MustCompile(`"reasoning_details":(\[[^\]]*\])`)
	for _, m := range re.FindAllSubmatch(raw, -1) {
		var frags []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m[1], &frags); err != nil {
			t.Fatalf("fixture fragment: %v", err)
		}
		for _, f := range frags {
			want.WriteString(f.Text)
		}
	}
	if want.Len() == 0 {
		t.Fatal("fixture carries no reasoning fragments — regenerate it")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	p := New(Config{Name: "test", BaseURL: srv.URL, APIKey: "test", Model: "deepseek/deepseek-v4-pro"})

	var reasoning []*llm.ReasoningPart
	for chunk, err := range p.Stream(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if chunk.Reasoning != nil {
			reasoning = append(reasoning, chunk.Reasoning)
		}
	}

	if len(reasoning) != 1 {
		t.Fatalf("got %d Reasoning chunks, want exactly 1 (one part carrying the whole array, like Generate)", len(reasoning))
	}
	var entries []map[string]any
	if err := json.Unmarshal(reasoning[0].Raw, &entries); err != nil {
		t.Fatalf("Reasoning.Raw is not the array shape: %v\n%s", err, reasoning[0].Raw)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want the fixture's single reasoning.text block", entries)
	}
	e := entries[0]
	if e["type"] != "reasoning.text" || e["format"] != "unknown" || e["index"] != float64(0) {
		t.Errorf("block identity not preserved: %v", e)
	}
	if got := e["text"]; got != want.String() {
		t.Errorf("reassembled text:\n got %q\nwant %q", got, want.String())
	}
}

// TestReasoningAccumulatorMerge pins the merge rules on the shapes that matter:
// multi-block ordering, fragment concatenation, and an encrypted block whose
// data must survive VERBATIM (a corrupted signature is worse than none).
func TestReasoningAccumulatorMerge(t *testing.T) {
	var acc reasoningAccumulator
	// A thinking phase (two text fragments), then an encrypted signature block
	// arriving complete in one delta — the Gemini shape.
	acc.add(json.RawMessage(`[{"type":"reasoning.text","text":"let me","format":"x","index":0}]`))
	acc.add(json.RawMessage(`[{"type":"reasoning.text","text":" think","format":"x","index":0}]`))
	acc.add(json.RawMessage(`[{"type":"reasoning.encrypted","data":"ENC_BLOB","format":"google-gemini-v1","id":"call_1","index":1}]`))
	acc.add(json.RawMessage(`garbage`)) // malformed fragment must not kill anything

	raw, ok := acc.raw()
	if !ok {
		t.Fatal("accumulator empty after adds")
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want [text block, encrypted block]", entries)
	}
	if entries[0]["text"] != "let me think" || entries[0]["index"] != float64(0) {
		t.Errorf("text block = %v, want concatenated 'let me think' at index 0", entries[0])
	}
	if entries[1]["data"] != "ENC_BLOB" || entries[1]["id"] != "call_1" {
		t.Errorf("encrypted block = %v, want verbatim ENC_BLOB/call_1", entries[1])
	}

	// Same index but different TYPE is a different block — never merged.
	var acc2 reasoningAccumulator
	acc2.add(json.RawMessage(`[{"type":"reasoning.text","text":"a","index":0},{"type":"reasoning.encrypted","data":"D","index":0}]`))
	raw2, _ := acc2.raw()
	var entries2 []map[string]any
	_ = json.Unmarshal(raw2, &entries2)
	if len(entries2) != 2 {
		t.Errorf("type+index keying broken: %v", entries2)
	}

	// No reasoning at all → no chunk (ok=false), so plain models are unchanged.
	var empty reasoningAccumulator
	if _, ok := empty.raw(); ok {
		t.Error("empty accumulator reports a trace")
	}
}
