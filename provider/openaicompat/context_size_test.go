package openaicompat

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/openai/openai-go"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// llama-server (rung-0 local serving) reports context overflow as HTTP 400
// {"code":400,"message":"request (N tokens) exceeds the available context
// size (M tokens), try increasing it","type":"exceed_context_size_error"}.
// Neither phrasing matched isContextLength, so the error classified as
// KindUnknown and the run DIED as provider_error instead of triggering the
// reactive eviction/compaction path (run 20260717-100318, backlog
// 2026-07-17). classify must map it to KindContextLength — via the message
// phrasing AND via the type tag alone (some bodies omit "message").

func TestClassifyLlamaServerContextOverflow(t *testing.T) {
	p := New(Config{Name: "ollama", BaseURL: "http://unused", APIKey: "test", Model: "m"})
	u, _ := url.Parse("http://unused")

	mkErr := func(message, typ string) *openai.Error {
		return &openai.Error{
			Message:    message,
			Type:       typ,
			StatusCode: http.StatusBadRequest,
			Request:    &http.Request{Method: http.MethodPost, URL: u},
			Response:   &http.Response{StatusCode: http.StatusBadRequest},
		}
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "request message phrasing",
			err: mkErr("request (35352 tokens) exceeds the available context size (32768 tokens), try increasing it",
				"exceed_context_size_error"),
		},
		{
			name: "request type tag only, empty message",
			err:  mkErr("", "exceed_context_size_error"),
		},
		{
			name: "stream error payload",
			err:  errors.New(`received error while streaming: {"code":400,"message":"request (35352 tokens) exceeds the available context size (32768 tokens), try increasing it"}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := p.classify(tc.err)
			var perr *llm.ProviderError
			if !errors.As(err, &perr) {
				t.Fatalf("classify = %v, want ProviderError", err)
			}
			if perr.Kind != llm.KindContextLength {
				t.Errorf("Kind = %v, want KindContextLength (err: %v)", perr.Kind, err)
			}
			if !errors.Is(err, llm.ErrContextLength) {
				t.Errorf("errors.Is(err, ErrContextLength) = false, want true")
			}
			if perr.Retryable {
				t.Errorf("context overflow must not be retryable-as-is: %+v", perr)
			}
		})
	}
}
