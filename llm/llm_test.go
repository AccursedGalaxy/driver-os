package llm

import (
	"context"
	"errors"
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
