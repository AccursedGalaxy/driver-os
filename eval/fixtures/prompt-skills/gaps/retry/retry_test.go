package retry

import (
	"errors"
	"testing"
)

// TestSucceedsThenLaterFailureIgnored: the first attempt succeeds, then a
// later transient failure occurs (simulating a downstream that degrades
// after already answering). WithRetry must still report the success.
func TestSucceedsThenLaterFailureIgnored(t *testing.T) {
	call := 0
	attempt := func() (string, error) {
		call++
		if call == 1 {
			return "ok", nil
		}
		return "", errors.New("transient failure")
	}
	val, err := WithRetry(attempt, 3)
	if err != nil {
		t.Fatalf("expected success, got error %v", err)
	}
	if val != "ok" {
		t.Fatalf("expected %q, got %q", "ok", val)
	}
}

func TestAllAttemptsFail(t *testing.T) {
	attempt := func() (string, error) { return "", errors.New("boom") }
	_, err := WithRetry(attempt, 3)
	if err == nil {
		t.Fatal("expected an error when every attempt fails")
	}
}
