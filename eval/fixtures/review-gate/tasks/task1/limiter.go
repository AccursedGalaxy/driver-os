package pipeline

import "time"

// Limiter is a sliding-window rate limiter: at most max events are admitted
// in any window of the given length. The window is half-open — an event that
// happened exactly `window` ago has expired and no longer counts against the
// limit.
type Limiter struct {
	max    int
	window time.Duration
	events []time.Time
}

// NewLimiter returns a limiter admitting at most max events per window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window}
}

// Allow reports whether an event at time t is admitted, recording it if so.
// Callers must pass non-decreasing times.
func (l *Limiter) Allow(t time.Time) bool {
	cutoff := t.Add(-l.window)
	kept := l.events[:0]
	for _, e := range l.events {
		if !e.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	l.events = kept
	if len(l.events) >= l.max {
		return false
	}
	l.events = append(l.events, t)
	return true
}
