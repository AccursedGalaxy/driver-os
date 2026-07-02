package openaicompat

// Regression tests for mid-stream error classification (diagnosed
// 2026-07-02): an SSE error EVENT reaches classify as a plain string error
// ("received error while streaming: {...}"), not an *openai.Error, so
// OpenRouter's transient {"code":504,"message":"Upstream idle timeout
// exceeded"} was Kind:unknown/Retryable:false and killed whole runs. The
// fixture stream mirrors the captured failure: content flows, then the error
// event arrives.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// streamWithError serves a healthy chunk followed by an SSE error event, and
// returns the text collected before the fault plus the terminal error.
func streamWithError(t *testing.T, errJSON string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"partial "}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"error":`+errJSON+`}`+"\n\n")
	}))
	defer srv.Close()

	p := New(Config{Name: "openrouter", BaseURL: srv.URL, APIKey: "test", Model: "m"})
	var text strings.Builder
	var last error
	for chunk, err := range p.Stream(context.Background(), llm.Request{Messages: []llm.Message{llm.User("hi")}}) {
		if err != nil {
			last = err
			break
		}
		text.WriteString(chunk.Text)
	}
	return text.String(), last
}

func TestStreamErrorUpstreamTimeoutIsRetryable(t *testing.T) {
	text, err := streamWithError(t, `{"code":504,"message":"Upstream idle timeout exceeded","metadata":{"error_type":"timeout"}}`)
	if text != "partial " {
		t.Errorf("text before fault = %q, want the streamed prefix", text)
	}
	var perr *llm.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *llm.ProviderError", err)
	}
	if !perr.Retryable || perr.StatusCode != 504 {
		t.Errorf("ProviderError = %+v, want Retryable 504", perr)
	}
}

func TestStreamErrorContextLengthRoutesToEviction(t *testing.T) {
	_, err := streamWithError(t, `{"code":400,"message":"This model's maximum context length is 8192 tokens"}`)
	if !errors.Is(err, llm.ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength (eviction, not a verbatim retry)", err)
	}
	var perr *llm.ProviderError
	if errors.As(err, &perr) && perr.Retryable {
		t.Errorf("context-length fault must not be retryable: %+v", perr)
	}
}

func TestStreamErrorAuthNotRetryable(t *testing.T) {
	_, err := streamWithError(t, `{"code":401,"message":"invalid api key"}`)
	if !errors.Is(err, llm.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	var perr *llm.ProviderError
	if errors.As(err, &perr) && perr.Retryable {
		t.Errorf("auth fault must not be retryable: %+v", perr)
	}
}

func TestClassifyUnexpectedEOFIsRetryable(t *testing.T) {
	p := New(Config{Name: "test", BaseURL: "http://unused", APIKey: "test", Model: "m"})
	var perr *llm.ProviderError
	if err := p.classify(io.ErrUnexpectedEOF); !errors.As(err, &perr) || !perr.Retryable {
		t.Fatalf("classify(ErrUnexpectedEOF) = %v, want a retryable ProviderError", err)
	}
}
