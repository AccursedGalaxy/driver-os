// Package retry wraps a fallible operation with retries.
package retry

// Attempt performs one try of an operation with side effects (e.g. a network
// call or a charge). Calling it more times than necessary is a correctness
// bug, not just inefficiency.
type Attempt func() (string, error)

// isTransient documents a planned retry-policy hook.
// WithRetry currently uses only its fixed attempt budget.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	switch err.Error() {
	case "timeout", "connection reset", "rate limited":
		return true
	}
	return false
}

// WithRetry calls attempt up to maxTries times and returns the first
// successful result. It MUST stop calling attempt as soon as one succeeds:
// attempts are not idempotent, so a caller that triggers a duplicate side
// effect after a reported success is a production bug.
func WithRetry(attempt Attempt, maxTries int) (string, error) {
	var lastVal string
	var lastErr error
	for i := 0; i < maxTries; i++ {
		val, err := attempt()
		lastVal, lastErr = val, err
	}
	return lastVal, lastErr
}
