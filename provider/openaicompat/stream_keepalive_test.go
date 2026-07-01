package openaicompat

// Regression tests for the OpenRouter keep-alive kill (diagnosed 2026-07-02):
// while a slow upstream spins up, OpenRouter emits SSE COMMENT lines
// (": OPENROUTER PROCESSING") followed by a blank line. The SDK's decoder
// (openai-go ssestream) skips the comment but still dispatches the blank line
// as an event with EMPTY data, and json.Unmarshal("") then kills the whole
// stream ("unexpected end of JSON input"). Slow/queued models — exactly the
// cheap reasoning tier — trigger the keep-alive, so their streams died while
// fast flagship streams never did. The fixture is a REAL captured
// deepseek-v4-pro stream (comments and all); the mid-stream variant covers a
// keep-alive arriving between content chunks (a reasoning pause).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

const keepaliveFixture = "testdata/openrouter-keepalive-deepseek.sse"

// wantSentence is the full assistant text inside the fixture stream.
const wantSentence = "The quick brown fox jumps over the lazy dog"

// replayStream serves raw SSE bytes and collects what Provider.Stream yields.
func replayStream(t *testing.T, raw []byte) (text string, finish llm.FinishReason, usage llm.Usage) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	p := New(Config{Name: "test", BaseURL: srv.URL, APIKey: "test", Model: "deepseek/deepseek-v4-pro"})
	var b strings.Builder
	for chunk, err := range p.Stream(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("reply with exactly this sentence: " + wantSentence)},
	}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if chunk.Done {
			finish = chunk.FinishReason
			usage = chunk.Usage
			continue
		}
		b.WriteString(chunk.Text)
	}
	return b.String(), finish, usage
}

func TestStreamSurvivesOpenRouterKeepaliveComments(t *testing.T) {
	raw, err := os.ReadFile(keepaliveFixture)
	if err != nil {
		t.Fatal(err)
	}
	text, finish, usage := replayStream(t, raw)
	if text != wantSentence {
		t.Errorf("assembled text:\n got %q\nwant %q", text, wantSentence)
	}
	if finish != llm.FinishStop {
		t.Errorf("finish = %v, want FinishStop", finish)
	}
	if usage.CompletionTokens == 0 {
		t.Errorf("usage lost: %+v", usage)
	}
}

func TestStreamSurvivesMidStreamKeepalive(t *testing.T) {
	raw, err := os.ReadFile(keepaliveFixture)
	if err != nil {
		t.Fatal(err)
	}
	// Inject a keep-alive between two content chunks: after the previous event's
	// terminating blank line the decoder's data buffer is empty, so the comment's
	// trailing blank line is exactly the empty-event dispatch that killed live
	// runs mid-generation.
	marker := "data: " // first content-bearing chunk boundary: split at a chunk carrying " quick"
	i := strings.Index(string(raw), `" quick"`)
	if i < 0 {
		t.Fatal("fixture changed: no ' quick' chunk")
	}
	start := strings.LastIndex(string(raw[:i]), marker)
	injected := string(raw[:start]) + ": OPENROUTER PROCESSING\n\n" + string(raw[start:])

	text, finish, usage := replayStream(t, []byte(injected))
	if text != wantSentence {
		t.Errorf("assembled text:\n got %q\nwant %q", text, wantSentence)
	}
	if finish != llm.FinishStop {
		t.Errorf("finish = %v, want FinishStop", finish)
	}
	if usage.CompletionTokens == 0 {
		t.Errorf("usage lost: %+v", usage)
	}
}

// TestSSECommentFilter pins the filter's line-level semantics directly.
func TestSSECommentFilter(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"comment then blank at stream start is fully swallowed",
			": OPENROUTER PROCESSING\n\ndata: {\"a\":1}\n\n",
			"data: {\"a\":1}\n\n"},
		{"repeated keep-alives are swallowed",
			": x\n\n: y\n\ndata: {}\n\n",
			"data: {}\n\n"},
		{"comment inside an event keeps the event's dispatch blank",
			"data: {}\n: keep\n\n",
			"data: {}\n\n"},
		{"mid-stream keep-alive between events",
			"data: {}\n\n: keep\n\ndata: {\"b\":2}\n\n",
			"data: {}\n\ndata: {\"b\":2}\n\n"},
		{"consecutive blank lines collapse to one dispatch",
			"data: {}\n\n\n\ndata: {}\n\n",
			"data: {}\n\ndata: {}\n\n"},
		{"CRLF keep-alive",
			": keep\r\n\r\ndata: {}\r\n\r\n",
			"data: {}\r\n\r\n"},
		{"leading blank lines are dropped",
			"\n\ndata: {}\n\n",
			"data: {}\n\n"},
		{"done marker passes through",
			"data: [DONE]\n\n",
			"data: [DONE]\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSECommentFilter(nopReadCloser{strings.NewReader(tc.in)})
			var b strings.Builder
			buf := make([]byte, 3) // tiny reads to exercise chunk boundaries
			for {
				n, err := f.Read(buf)
				b.Write(buf[:n])
				if err != nil {
					break
				}
			}
			if b.String() != tc.want {
				t.Errorf("filtered:\n got %q\nwant %q", b.String(), tc.want)
			}
		})
	}
}

type nopReadCloser struct{ *strings.Reader }

func (nopReadCloser) Close() error { return nil }
