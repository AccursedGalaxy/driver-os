package pipeline

import (
	"testing"
	"time"
)

func TestLimiterDeniesInsideWindow(t *testing.T) {
	l := NewLimiter(1, time.Second)
	t0 := time.Unix(1000, 0)
	if !l.Allow(t0) {
		t.Fatal("first event must be admitted")
	}
	if l.Allow(t0.Add(500 * time.Millisecond)) {
		t.Fatal("second event inside the window must be denied")
	}
}

func TestLimiterBoundaryExpiry(t *testing.T) {
	l := NewLimiter(1, time.Second)
	t0 := time.Unix(1000, 0)
	if !l.Allow(t0) {
		t.Fatal("first event must be admitted")
	}
	// The window is half-open: at t0+1s the event at t0 has expired.
	if !l.Allow(t0.Add(time.Second)) {
		t.Fatal("event exactly one window later must be admitted (half-open window)")
	}
}

func TestLimiterSteadyRate(t *testing.T) {
	l := NewLimiter(2, time.Second)
	t0 := time.Unix(2000, 0)
	admitted := 0
	// One event per 500ms for 4 seconds: every event should be admitted,
	// because at each tick the event from one full window ago has expired.
	for i := 0; i < 8; i++ {
		if l.Allow(t0.Add(time.Duration(i) * 500 * time.Millisecond)) {
			admitted++
		}
	}
	if admitted != 8 {
		t.Fatalf("admitted %d of 8 events at exactly the allowed rate", admitted)
	}
}
