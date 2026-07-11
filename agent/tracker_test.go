package agent

import (
	"strings"
	"testing"
)

// ---- Green-repeat nudge state machine ----

func TestGreenRepeatNudgeFiresAtThreeIdenticalGreens(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)

	// Three identical passing runs with no mutations in between.
	green := "exit 0 (5ms)\nstdout:\nok"
	for i := 0; i < 3; i++ {
		tr.observeRun(green)
	}
	if tr.greenRepeat != 3 {
		t.Fatalf("greenRepeat = %d, want 3 after three identical greens", tr.greenRepeat)
	}
	if !tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should fire at count 3")
	}
	// Latching: a second call must return false.
	if tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should be latched — second call returned true")
	}
}

func TestGreenRepeatNudgeDoesNotFireAtTwo(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)
	green := "exit 0 (5ms)\nstdout:\nok"
	tr.observeRun(green)
	tr.observeRun(green)
	if tr.greenRepeat != 2 {
		t.Fatalf("greenRepeat = %d, want 2", tr.greenRepeat)
	}
	if tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should NOT fire at count 2")
	}
}

func TestGreenRepeatNudgeResetsOnMutation(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)
	green := "exit 0 (5ms)\nstdout:\nok"

	// Two greens, then a mutating action, then a third green.
	tr.observeRun(green)              // count=1
	tr.observeRun(green)              // count=2
	tr.observeAction(1, "write_file") // mutation resets -> 0
	tr.observeRun(green)              // count=1 (restarted)
	if tr.greenRepeat != 1 {
		t.Fatalf("greenRepeat = %d, want 1 after mutation reset", tr.greenRepeat)
	}
	if tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should NOT fire — mutation reset the counter")
	}
}

func TestGreenRepeatNudgeResetsOnFailingRun(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)
	green := "exit 0 (5ms)\nstdout:\nok"
	fail := "exit 1 (5ms)\nstderr:\nboom"

	tr.observeRun(green) // count=1
	tr.observeRun(green) // count=2
	tr.observeRun(fail)  // failing run resets -> 0
	tr.observeRun(green) // count=1 (restarted)
	if tr.greenRepeat != 1 {
		t.Fatalf("greenRepeat = %d, want 1 after failure reset", tr.greenRepeat)
	}
	if tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should NOT fire — failing run reset the counter")
	}
}

func TestGreenRepeatNudgeDoesNotAccumulateOnDifferentGreen(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)

	// Different fingerprints for different green commands — distinct non-numeric
	// body text so runFingerprint produces different results.
	greenA := "exit 0 (5ms)\nstdout:\nok\nall tests passed"
	greenB := "exit 0 (5ms)\nstdout:\nok\nbenchmarks passed" // different body text

	tr.observeRun(greenA) // count=1, fp=A
	tr.observeRun(greenB) // different fp -> count=1, fp=B
	if tr.greenRepeat != 1 {
		t.Fatalf("greenRepeat = %d, want 1 after B (different from A)", tr.greenRepeat)
	}
	tr.observeRun(greenB) // count=2
	tr.observeRun(greenB) // count=3
	if tr.greenRepeat != 3 {
		t.Fatalf("greenRepeat = %d, want 3 after B,B,B", tr.greenRepeat)
	}
	if !tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should fire after 3 identical greenB runs")
	}
}

func TestGreenRepeatNudgeFiresAtMostOnce(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)
	green := "exit 0 (5ms)\nstdout:\nok"

	for i := 0; i < 5; i++ {
		tr.observeRun(green)
	}
	// First call fires.
	if !tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should fire at count 5")
	}
	// Second call is latched out.
	if tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should be latched — second call returned true")
	}
	// Counter still advances.
	tr.observeRun(green)
	if tr.greenRepeat != 6 {
		t.Fatalf("greenRepeat = %d, want 6 (counter advances even after nudge)", tr.greenRepeat)
	}
}

func TestGreenRepeatNudgedFlagPreventsRefire(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)

	// Manually set state to simulate prior fire.
	tr.greenRepeat = 3
	tr.greenNudged = true

	if tr.greenRepeatNudge() {
		t.Fatal("greenRepeatNudge should return false when already nudged")
	}
}

func TestGreenRepeatNudgeDoesNotInterfereWithStagnant(t *testing.T) {
	// The green-repeat counter and the stagnant (failing-run) counter are
	// independent: a green run resets stagnant, a failing run resets green-repeat.
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)
	fail := "exit 1 (5ms)\nstderr:\nboom"
	green := "exit 0 (5ms)\nstdout:\nok"

	// Two identical failures.
	tr.observeRun(fail) // stagnant=1
	tr.observeRun(fail) // stagnant=2

	// A green run resets stagnant but starts green-repeat.
	tr.observeRun(green) // stagnant=0, greenRepeat=1
	if tr.stagnant != 0 {
		t.Errorf("stagnant = %d, want 0 (reset by green)", tr.stagnant)
	}
	if tr.greenRepeat != 1 {
		t.Errorf("greenRepeat = %d, want 1", tr.greenRepeat)
	}

	// A failing run resets green-repeat.
	tr.observeRun(fail) // greenRepeat=0, stagnant=1
	if tr.greenRepeat != 0 {
		t.Errorf("greenRepeat = %d, want 0 (reset by failure)", tr.greenRepeat)
	}
	if tr.stagnant != 1 {
		t.Errorf("stagnant = %d, want 1", tr.stagnant)
	}
}
func TestEscalatingRepeatNudgeText(t *testing.T) {
	got := map[int]string{}
	for _, count := range []int{0, 1, 2, 3, 4, 5, 6, 7} {
		got[count] = escalatingRepeatNudge(count)
	}
	for _, count := range []int{0, 1, 2, 6, 7} {
		if got[count] != "" {
			t.Errorf("escalatingRepeatNudge(%d) = %q, want empty", count, got[count])
		}
	}
	checks := map[int][]string{
		3: {"[harness:", "identical", "3", "finish", "genuinely different action"},
		4: {"[harness:", "4 identical", "will not change the outcome"},
		5: {"[harness:", "5th identical", "next identical turn ENDS the run", "FINAL answer", "NOW"},
	}
	for count, wants := range checks {
		if got[count] == "" {
			t.Fatalf("escalatingRepeatNudge(%d) returned empty", count)
		}
		for _, want := range wants {
			if !strings.Contains(got[count], want) {
				t.Errorf("escalatingRepeatNudge(%d) = %q, missing %q", count, got[count], want)
			}
		}
	}
	if got[3] == got[4] || got[4] == got[5] || got[3] == got[5] {
		t.Fatalf("nudges at 3/4/5 must be distinct: %#v", got)
	}
}

func TestToolObservationRepeatCountIgnoresDecoratingNudges(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 20, DefaultTerminationPolicy(), nil)
	raw := "run {\"command\":\"go test ./...\"}\nOBSERVATION:\nexit 0 (1ms)\nstdout:\nok"
	decorated := raw + churnNudge + greenRepeatNudgeText + escalatingRepeatNudge(3)
	for want := 1; want <= 3; want++ {
		kill, got := tr.observeToolObservation(raw)
		if kill {
			t.Fatalf("observeToolObservation killed at count %d", got)
		}
		if got != want {
			t.Fatalf("raw repeat count = %d, want %d", got, want)
		}
	}
	if decorated == raw {
		t.Fatal("test setup failed: decorated fingerprint did not change")
	}
	// The loop must feed observeToolObservation the same raw string even on turns
	// where churn/green/escalating nudges are sent to the model. If it fed the
	// decorated observation instead, this next call would reset the counter to 1.
	_, got := tr.observeToolObservation(raw)
	if got != 4 {
		t.Fatalf("repeat count after a decorated model observation = %d, want 4 (fingerprint stayed raw)", got)
	}
}
