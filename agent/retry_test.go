package agent

// Regression tests for the transient-provider-fault retry (diagnosed
// 2026-07-02): OpenRouter killed a deepseek stream mid-flight with
// {"code":504,"message":"Upstream idle timeout exceeded"} and the unretried
// fault ended the WHOLE run as provider_error, discarding 20+ iterations of
// progress (run 20260702-204848). generateWithRetry re-sends a bounded number
// of times on Retryable provider errors and leaves every non-retryable error
// untouched for the callers that type on them (eviction, cancel, auth).

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// flaky fails its first n Generate calls with err, then answers.
type flaky struct {
	failures int
	err      error
	calls    int
}

func (f *flaky) Name() string                   { return "flaky" }
func (f *flaky) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (f *flaky) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("flaky")
}

func (f *flaky) Generate(context.Context, llm.Request) (*llm.Response, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, f.err
	}
	return &llm.Response{Content: []llm.ContentPart{llm.Text("ok")}, FinishReason: llm.FinishStop}, nil
}

func quickBackoff(t *testing.T) {
	t.Helper()
	old := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = old })
}

func TestGenerateRetriesTransientFault(t *testing.T) {
	quickBackoff(t)
	transient := &llm.ProviderError{Provider: "test", Kind: llm.KindUnknown, StatusCode: 504, Retryable: true}
	m := &flaky{failures: 2, err: transient}
	resp, err := generateWithRetry(context.Background(), Config{Model: m, Obs: nopObserver{}}, llm.Request{})
	if err != nil {
		t.Fatalf("err = %v, want recovery after retries", err)
	}
	if resp.Text() != "ok" || m.calls != 3 {
		t.Fatalf("resp = %q after %d calls, want \"ok\" after 3", resp.Text(), m.calls)
	}
}

func TestGenerateRetryGivesUpAfterCap(t *testing.T) {
	quickBackoff(t)
	transient := &llm.ProviderError{Provider: "test", Kind: llm.KindUnknown, Retryable: true}
	m := &flaky{failures: 10, err: transient}
	_, err := generateWithRetry(context.Background(), Config{Model: m, Obs: nopObserver{}}, llm.Request{})
	if !errors.Is(err, transient) {
		t.Fatalf("err = %v, want the transient error after exhausting retries", err)
	}
	if want := 1 + transientMaxRetries; m.calls != want {
		t.Fatalf("calls = %d, want %d (bounded, billed attempts)", m.calls, want)
	}
}

func TestGenerateNoRetryOnNonRetryable(t *testing.T) {
	quickBackoff(t)
	for name, fault := range map[string]error{
		"context_length": &llm.ProviderError{Provider: "test", Kind: llm.KindContextLength},
		"canceled":       &llm.ProviderError{Provider: "test", Kind: llm.KindCanceled},
		"auth":           &llm.ProviderError{Provider: "test", Kind: llm.KindAuth},
	} {
		m := &flaky{failures: 10, err: fault}
		_, err := generateWithRetry(context.Background(), Config{Model: m, Obs: nopObserver{}}, llm.Request{})
		if !errors.Is(err, fault) || m.calls != 1 {
			t.Errorf("%s: err = %v after %d calls, want the fault untouched after 1 call", name, err, m.calls)
		}
	}
}
