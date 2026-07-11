package agent

import (
	"testing"
)

func TestRunFingerprintAssertionExemption(t *testing.T) {
	// a. Two formatRun-shaped observations identical except an assertion value
	// ("want 42, got 17" vs "want 42, got 41") => runFingerprint returns DIFFERENT strings.
	obs1 := "exit 1 (10ms)\nstdout:\nwant 42, got 17"
	obs2 := "exit 1 (20ms)\nstdout:\nwant 42, got 41"
	fp1 := runFingerprint(obs1)
	fp2 := runFingerprint(obs2)
	if fp1 == fp2 {
		t.Errorf("fingerprints should differ for different assertion values:\n%q\n%q", fp1, fp2)
	}

	// b. Two observations identical except the first-line duration ("exit 1 (0.03s)" vs "exit 1 (1.2s)")
	// and a volatile non-assertion number in the body (e.g. "/tmp/test123" vs "/tmp/test456")
	// => SAME fingerprint.
	obs3 := "exit 1 (0.03s)\nstderr:\nfailed at /tmp/test123"
	obs4 := "exit 1 (1.2s)\nstderr:\nfailed at /tmp/test456"
	fp3 := runFingerprint(obs3)
	fp4 := runFingerprint(obs4)
	if fp3 != fp4 {
		t.Errorf("fingerprints should be identical for volatile numbers:\n%q\n%q", fp3, fp4)
	}
}

func TestStagnantDetectorAssertionExemption(t *testing.T) {
	// c. Through the detector: feed turnTracker.observeRun three failing observations
	// that differ ONLY in the got-value of an assertion line => no kill (returns kill=false each time).
	tracker := newTurnTrackerT(Config{}, 0, DefaultTerminationPolicy(), nil)
	obsA := "exit 1 (10ms)\nwant 42, got 1"
	obsB := "exit 1 (10ms)\nwant 42, got 2"
	obsC := "exit 1 (10ms)\nwant 42, got 3"

	if kill, _ := tracker.observeRun(obsA); kill {
		t.Error("first failure should not kill")
	}
	if kill, _ := tracker.observeRun(obsB); kill {
		t.Error("second (different) failure should not kill")
	}
	if kill, _ := tracker.observeRun(obsC); kill {
		t.Error("third (different) failure should not kill")
	}

	// Feed three byte-identical failing observations => the third returns kill=true (unchanged strict behavior).
	tracker2 := newTurnTrackerT(Config{}, 0, DefaultTerminationPolicy(), nil)
	obsIdentical := "exit 1 (10ms)\nbody"
	if kill, _ := tracker2.observeRun(obsIdentical); kill {
		t.Error("first failure should not kill")
	}
	if kill, _ := tracker2.observeRun(obsIdentical); kill {
		t.Error("second failure should not kill")
	}
	if kill, _ := tracker2.observeRun(obsIdentical); !kill {
		t.Error("third identical failure SHOULD kill")
	}
}
