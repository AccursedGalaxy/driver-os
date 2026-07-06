package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestParallelSafe(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"read_file", true},
		{"search", true},
		{"list_dir", true},
		{"go_doc", true},
		{"run", false},
		{"write_file", false},
		{"edit_file", false},
		{"other", false},
	}
	for _, tc := range cases {
		if got := parallelSafe(tc.name); got != tc.want {
			t.Errorf("parallelSafe(%q) = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestPrefetchConcurrent(t *testing.T) {
	var count int32
	target := int32(3)
	barrier := make(chan struct{})
	tool := Tool{
		RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) {
			newVal := atomic.AddInt32(&count, 1)
			if newVal == target {
				close(barrier)
			}
			<-barrier
			return "ok", nil
		},
	}
	tools := map[string]Tool{
		"read_file": tool,
	}
	calls := []llm.ToolCallPart{
		{Name: "read_file", ID: "1"},
		{Name: "read_file", ID: "2"},
		{Name: "read_file", ID: "3"},
	}

	done := make(chan struct{})
	go func() {
		results, k := prefetchLeadingReadOnly(context.Background(), tools, calls, false)
		if k != 3 {
			t.Errorf("expected k=3, got %d", k)
		}
		if len(results) != 3 {
			t.Errorf("expected 3 results, got %d", len(results))
		}
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: tools did not run concurrently")
	}
}

func TestPrefetchOrdering(t *testing.T) {
	tools := map[string]Tool{
		"read_file": {RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) { return "obs1", nil }},
		"search":    {RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) { return "obs2", nil }},
	}
	calls := []llm.ToolCallPart{
		{Name: "read_file", ID: "1"},
		{Name: "search", ID: "2"},
	}
	results, k := prefetchLeadingReadOnly(context.Background(), tools, calls, false)
	if k != 2 {
		t.Fatalf("expected k=2, got %d", k)
	}
	if results[0].obs != "obs1" || results[1].obs != "obs2" {
		t.Errorf("ordering mismatch: got [%q, %q]", results[0].obs, results[1].obs)
	}
}

func TestPrefetchPrefixStopsAtUnsafe(t *testing.T) {
	tools := map[string]Tool{
		"read_file":  {RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) { return "read", nil }},
		"run":        {RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) { return "run", nil }},
		"write_file": {RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) { return "write", nil }},
	}

	// Case 1: [read, read, run, read] -> k=2
	calls1 := []llm.ToolCallPart{
		{Name: "read_file", ID: "1"},
		{Name: "read_file", ID: "2"},
		{Name: "run", ID: "3"},
		{Name: "read_file", ID: "4"},
	}
	_, k1 := prefetchLeadingReadOnly(context.Background(), tools, calls1, false)
	if k1 != 2 {
		t.Errorf("Case 1: expected k=2, got %d", k1)
	}

	// Case 2: [write, read, read] -> k=0
	calls2 := []llm.ToolCallPart{
		{Name: "write_file", ID: "1"},
		{Name: "read_file", ID: "2"},
		{Name: "read_file", ID: "3"},
	}
	_, k2 := prefetchLeadingReadOnly(context.Background(), tools, calls2, false)
	if k2 != 0 {
		t.Errorf("Case 2: expected k=0, got %d", k2)
	}

	// Case 3: [read] -> k=0
	calls3 := []llm.ToolCallPart{
		{Name: "read_file", ID: "1"},
	}
	results3, k3 := prefetchLeadingReadOnly(context.Background(), tools, calls3, false)
	if k3 != 0 || results3 != nil {
		t.Errorf("Case 3: expected k=0 and nil results, got k=%d", k3)
	}
}

func TestPrefetchSkipLeadingDisables(t *testing.T) {
	tools := map[string]Tool{
		"read_file": {RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) { return "read", nil }},
	}
	calls := []llm.ToolCallPart{
		{Name: "read_file", ID: "1"},
		{Name: "read_file", ID: "2"},
	}
	results, k := prefetchLeadingReadOnly(context.Background(), tools, calls, true) // skipLeading = true
	if k != 0 || results != nil {
		t.Errorf("expected k=0 when skipLeading is true, got k=%d", k)
	}
}

func TestPrefetchDuration(t *testing.T) {
	sleepDur := 30 * time.Millisecond
	tools := map[string]Tool{
		"read_file": {RunJSON: func(ctx context.Context, args json.RawMessage) (string, error) {
			time.Sleep(sleepDur)
			return "read", nil
		}},
	}
	calls := []llm.ToolCallPart{
		{Name: "read_file", ID: "1"},
		{Name: "read_file", ID: "2"},
	}
	results, k := prefetchLeadingReadOnly(context.Background(), tools, calls, false)
	if k != 2 {
		t.Fatalf("expected k=2, got %d", k)
	}
	for i, res := range results {
		if res.dur < sleepDur {
			t.Errorf("result %d duration %v < sleep %v", i, res.dur, sleepDur)
		}
	}
}
