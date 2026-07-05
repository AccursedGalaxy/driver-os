package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
	"testing"
)

func TestMessageHelpersAndText(t *testing.T) {
	if got := User("hi").Text(); got != "hi" {
		t.Fatalf("User text = %q, want %q", got, "hi")
	}
	if r := System("sys").Role; r != RoleSystem {
		t.Fatalf("System role = %q, want %q", r, RoleSystem)
	}

	m := UserParts(Text("a"), ImagePart{URL: "x"}, Text("b"))
	if got := m.Text(); got != "ab" {
		t.Fatalf("UserParts text = %q, want %q (non-text parts ignored)", got, "ab")
	}
}

func TestResponseText(t *testing.T) {
	r := &Response{Content: []ContentPart{Text("foo"), Text("bar")}}
	if got := r.Text(); got != "foobar" {
		t.Fatalf("Response.Text = %q, want %q", got, "foobar")
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	a, b := &fakeProvider{name: "a"}, &fakeProvider{name: "b"}
	reg.Add("a", a).Add("b", b)

	if got, ok := reg.Get("a"); !ok || got != a {
		t.Fatalf("Get(a) = %v, %v", got, ok)
	}
	if _, ok := reg.Get("missing"); ok {
		t.Fatal("Get(missing) should report ok=false")
	}
	if got := reg.Names(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Names = %v, want sorted [a b]", got)
	}
	if got := reg.All(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("All = %v, want [a b] in name order", got)
	}
}

func TestMustGetPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustGet(missing) should panic")
		}
	}()
	NewRegistry().MustGet("nope")
}

func TestProviderErrorIsMatchesByKind(t *testing.T) {
	err := &ProviderError{Provider: "xai", Kind: KindRateLimit, StatusCode: 429, Err: errors.New("boom")}

	if !errors.Is(err, ErrRateLimit) {
		t.Fatal("errors.Is should match ErrRateLimit by Kind")
	}
	if errors.Is(err, ErrAuth) {
		t.Fatal("errors.Is should not match a different Kind")
	}

	// Underlying error stays reachable.
	if errors.Unwrap(err).Error() != "boom" {
		t.Fatal("Unwrap should return the wrapped error")
	}

	var pe *ProviderError
	if !errors.As(err, &pe) || pe.StatusCode != 429 {
		t.Fatalf("errors.As should extract the ProviderError with status 429, got %+v", pe)
	}
}

// fakeProvider is a no-op Provider for registry tests.
type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string               { return f.name }
func (f *fakeProvider) Capabilities() Capabilities { return Capabilities{} }
func (f *fakeProvider) Generate(_ context.Context, _ Request) (*Response, error) {
	return nil, nil
}
func (f *fakeProvider) Stream(context.Context, Request) iter.Seq2[Chunk, error] {
	return UnsupportedStream(f.name)
}

// --- Usage JSON shape ---

func TestUsageMarshalSnakeCase(t *testing.T) {
	u := Usage{
		PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14,
		CachedTokens: 4, ReasoningTokens: 2,
	}
	data := marsh(u)
	// Every key must be snake_case; no PascalCase leakage.
	if containsKey(data, "PromptTokens") || containsKey(data, "CompletionTokens") ||
		containsKey(data, "TotalTokens") || containsKey(data, "CachedTokens") ||
		containsKey(data, "ReasoningTokens") {
		t.Fatalf("marshal leaked PascalCase keys:\n%s", data)
	}
	for _, want := range []string{`"prompt_tokens":11`, `"completion_tokens":3`, `"total_tokens":14`,
		`"cached_tokens":4`, `"reasoning_tokens":2`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("marshal missing %q in:\n%s", want, data)
		}
	}
}

func TestUsageUnmarshalNewShapeRoundTrip(t *testing.T) {
	orig := Usage{
		PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14,
		CachedTokens: 4, ReasoningTokens: 2,
	}
	data := marsh(orig)
	var got Usage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch: %+v != %+v", got, orig)
	}
}

func TestUsageUnmarshalLegacyShape(t *testing.T) {
	// Regression: the 2026-07 corpus writes PascalCase keys; readers must
	// still decode them correctly — not silently zero them.
	legacy := `{"PromptTokens":11,"CompletionTokens":3,"TotalTokens":14,"CachedTokens":4,"ReasoningTokens":2}`
	var got Usage
	if err := json.Unmarshal([]byte(legacy), &got); err != nil {
		t.Fatal(err)
	}
	want := Usage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14, CachedTokens: 4, ReasoningTokens: 2}
	if got != want {
		t.Fatalf("legacy decode: got %+v, want %+v", got, want)
	}
}

func TestUsageUnmarshalSnakeWinsWhenBothPresent(t *testing.T) {
	// When a (possibly hand-edited) record carries both shapes, snake_case
	// wins — the new canonical shape is the authority.
	mixed := `{"prompt_tokens":1,"PromptTokens":999,"completion_tokens":2,"CompletionTokens":888,"total_tokens":3,"TotalTokens":777,"cached_tokens":4,"CachedTokens":666,"reasoning_tokens":5,"ReasoningTokens":555}`
	var got Usage
	if err := json.Unmarshal([]byte(mixed), &got); err != nil {
		t.Fatal(err)
	}
	want := Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, CachedTokens: 4, ReasoningTokens: 5}
	if got != want {
		t.Fatalf("mixed-shape decode: got %+v, want %+v", got, want)
	}
}

func TestUsageUnmarshalPartialFields(t *testing.T) {
	// Absent fields stay zero.
	var got Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":5}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 5 || got.CompletionTokens != 0 || got.TotalTokens != 0 ||
		got.CachedTokens != 0 || got.ReasoningTokens != 0 {
		t.Fatalf("partial decode: got %+v", got)
	}
}

func TestUsageUnmarshalCost(t *testing.T) {
	var got Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":5,"cost":0.42}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cost != 0.42 {
		t.Fatalf("cost decode: got %v, want 0.42", got.Cost)
	}

	got = Usage{}
	if err := json.Unmarshal([]byte(`{"prompt_tokens":5}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cost != 0 {
		t.Fatalf("missing cost: got %v, want 0", got.Cost)
	}
}

func TestUsageUnmarshalEmpty(t *testing.T) {
	var got Usage
	if err := json.Unmarshal([]byte(`{}`), &got); err != nil {
		t.Fatal(err)
	}
	if got != (Usage{}) {
		t.Fatalf("empty object: got %+v, want zero", got)
	}
}

// --- tiny test helpers ---

func marsh(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func containsKey(data []byte, key string) bool {
	return bytes.Contains(data, []byte(`"`+key+`"`))
}
