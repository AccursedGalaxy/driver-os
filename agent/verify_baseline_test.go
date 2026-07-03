package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

type noteSpy struct {
	nopObserver
	notes []string
}

func (s *noteSpy) Note(msg string) {
	s.notes = append(s.notes, msg)
}

func TestVerifyBaseline(t *testing.T) {
	for _, tc := range []struct {
		name               string
		verifyCmd          string
		skipVerifyBaseline bool
		finalAnswer        string
		wantBaselineRed    bool
		wantBaselineOut    bool
		wantNote           bool
		wantReasonNote     bool
	}{
		{
			name:            "baseline red recorded and proceeds",
			verifyCmd:       "false",
			finalAnswer:     "done",
			wantBaselineRed: true,
			wantBaselineOut: true,
			wantNote:        true,
			wantReasonNote:  true, // "false" always fails, so it will be unverified
		},
		{
			name:            "baseline green records nothing",
			verifyCmd:       "true",
			finalAnswer:     "done",
			wantBaselineRed: false,
			wantNote:        false,
			wantReasonNote:  false, // "true" passes, so it will be Answered
		},
		{
			name:               "skip baseline runs nothing",
			verifyCmd:          "false",
			skipVerifyBaseline: true,
			finalAnswer:        "done",
			wantBaselineRed:    false,
			wantNote:           false,
			wantReasonNote:     false, // baseline skipped, so no annotation
		},
		{
			name:            "unverified final with red baseline gets annotation",
			verifyCmd:       "false",
			finalAnswer:     "answer: done", // text loop needs "answer:" or it's just prose
			wantBaselineRed: true,
			wantBaselineOut: true,
			wantNote:        true,
			wantReasonNote:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &noteSpy{}
			cfg := Config{
				Model:              &nativeScript{turns: [][]llm.ContentPart{{llm.Text(tc.finalAnswer)}}},
				Sandbox:            sbWith(t, nil),
				Task:               "test task",
				VerifyCmd:          tc.verifyCmd,
				SkipVerifyBaseline: tc.skipVerifyBaseline,
				Obs:                spy,
			}
			// Use RunNative for simplicity in testing
			res, err := RunNative(context.Background(), cfg)
			if err != nil {
				t.Fatalf("RunNative: %v", err)
			}

			if res.VerifyBaselineRed != tc.wantBaselineRed {
				t.Errorf("VerifyBaselineRed = %v, want %v", res.VerifyBaselineRed, tc.wantBaselineRed)
			}
			if tc.wantBaselineOut && !strings.Contains(res.VerifyBaselineOut, "exit 1") {
				t.Errorf("VerifyBaselineOut = %q, want it to contain 'exit 1'", res.VerifyBaselineOut)
			}
			if !tc.wantBaselineOut && res.VerifyBaselineOut != "" {
				t.Errorf("VerifyBaselineOut = %q, want empty", res.VerifyBaselineOut)
			}

			hasNote := false
			for _, n := range spy.notes {
				if strings.Contains(n, "verify baseline: RED") {
					hasNote = true
					break
				}
			}
			if hasNote != tc.wantNote {
				t.Errorf("Observer saw 'verify baseline: RED' note = %v, want %v", hasNote, tc.wantNote)
			}

			if tc.wantReasonNote {
				if res.Outcome != Unverified {
					t.Errorf("Outcome = %s, want Unverified", res.Outcome)
				}
				wantSub := "note: the verify command was ALREADY failing"
				if !strings.Contains(res.Reason, wantSub) {
					t.Errorf("Reason = %q, want it to contain %q", res.Reason, wantSub)
				}
			} else if res.Outcome == Unverified {
				wantSub := "note: the verify command was ALREADY failing"
				if strings.Contains(res.Reason, wantSub) {
					t.Errorf("Reason = %q, want it NOT to contain %q", res.Reason, wantSub)
				}
			}
		})
	}
}

// TestAbortOnRedBaselineNative proves that with AbortOnRedBaseline=true and a
// red baseline, RunNative returns Unverified with zero model calls.
func TestAbortOnRedBaselineNative(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("should not be called")}}}
	cfg := Config{
		Model:              ns,
		Sandbox:            sbWith(t, nil),
		Task:               "unrelated task",
		VerifyCmd:          "false",
		AbortOnRedBaseline: true,
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Unverified {
		t.Errorf("Outcome = %s, want Unverified", res.Outcome)
	}
	if res.Iterations != 0 {
		t.Errorf("Iterations = %d, want 0", res.Iterations)
	}
	if !res.VerifyBaselineRed {
		t.Error("VerifyBaselineRed = false, want true")
	}
	if res.VerifyBaselineOut == "" {
		t.Error("VerifyBaselineOut is empty, want the 'false' output")
	}
	if !strings.Contains(res.Reason, "unsatisfiable") {
		t.Errorf("Reason = %q, want it to say 'unsatisfiable'", res.Reason)
	}
	if !strings.Contains(res.Reason, "false") {
		t.Errorf("Reason = %q, should contain the verify command", res.Reason)
	}
	if len(ns.calls) != 0 {
		t.Errorf("model called %d times, want 0", len(ns.calls))
	}
}

// TestAbortOnRedBaselineText proves the same for the text loop.
func TestAbortOnRedBaselineText(t *testing.T) {
	sp := &scripted{replies: []string{"should not be called"}}
	cfg := Config{
		Model:              sp,
		Sandbox:            sbWith(t, nil),
		Task:               "unrelated task",
		VerifyCmd:          "false",
		AbortOnRedBaseline: true,
	}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != Unverified {
		t.Errorf("Outcome = %s, want Unverified", res.Outcome)
	}
	if res.Iterations != 0 {
		t.Errorf("Iterations = %d, want 0", res.Iterations)
	}
	if len(sp.calls) != 0 {
		t.Errorf("model called %d times, want 0", len(sp.calls))
	}
}

// TestBaselineWarningInSeedMessage proves that with the default mode (on/warn),
// a red baseline injects the warning into the first user message.
func TestBaselineWarningInSeedMessage(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
	cfg := Config{
		Model:     ns,
		Sandbox:   sbWith(t, nil),
		Task:      "fix the build",
		VerifyCmd: "false",
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("no messages in result")
	}
	first := res.Messages[0].Text()
	if !strings.Contains(first, "PRE-FLIGHT VERIFY BASELINE") {
		t.Errorf("first user message does not contain baseline warning: %s", first)
	}
}

// TestVerifyContinueBaselineIdenticalFingerprint proves that when the finish-time
// VerifyCmd failure is the SAME failure as the pre-existing red baseline (identical
// fingerprint), VerifyContinue grants exactly ONE rescue round: the first identical
// failure is still rejected into more work, and only a SECOND identical failure
// — after a full repair round — goes terminal as Unverified.
func TestVerifyContinueBaselineIdenticalFingerprint(t *testing.T) {
	// Use a verify command whose output is deterministic (no timestamps, no random
	// values in the body). "false" produces "exit 1 (...) (no output)" which
	// fingerprints identically run-to-run.
	// The model produces two no-tool-call finish turns: the first is rejected via
	// VerifyContinue (grace round), the second is terminal.
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("The task is unrelated — the verify command was already failing before any changes.")},
		{llm.Text("Still failing — nothing I can do.")},
	}}
	cfg := Config{
		Model:          ns,
		Sandbox:        sbWith(t, nil),
		Task:           "unrelated task",
		VerifyCmd:      "false",
		VerifyContinue: true,
		MaxIterations:  10,
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Unverified {
		t.Errorf("Outcome = %s, want Unverified", res.Outcome)
	}
	if res.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2 (first finish was rejected, second was terminal)", res.Iterations)
	}
	if !strings.Contains(res.Reason, "note: the verify command was ALREADY failing") {
		t.Errorf("Reason = %q, want it to mention the pre-existing red baseline", res.Reason)
	}
	if !res.VerifyBaselineRed {
		t.Error("VerifyBaselineRed = false, want true")
	}
	if len(ns.calls) != 2 {
		t.Errorf("model called %d times, want 2 (first rejected into grace round, second terminal)", len(ns.calls))
	}
}

// TestVerifyContinueBaselineDifferentFingerprint is the control: baseline red, but
// the finish-time verify failure has a DIFFERENT fingerprint (the solver broke
// something new). VerifyContinue=true still rejects the finish and loops — unchanged
// behavior from before the baseline-identical guard.
func TestVerifyContinueBaselineDifferentFingerprint(t *testing.T) {
	// VerifyCmd checks for a marker file that does NOT exist at baseline.
	// The solver writes it on turn 1 with content that changes the verify output,
	// then finishes on turn 2. The fingerprint differs from baseline → continue.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "marker.txt", "content": "hello"})},
		{llm.Text("done — I wrote the marker file but the verify still fails")}, // first finish: different fingerprint
		{llm.Text("done")}, // after continue feedback: second finish
	}
	ns := &nativeScript{turns: turns}
	cfg := Config{
		Model:          ns,
		Sandbox:        sbWith(t, nil),
		Task:           "fix the build",
		VerifyCmd:      "sh -c 'cat marker.txt 2>&1; exit 1'",
		VerifyContinue: true,
		MaxIterations:  10,
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	// The loop CONTINUES past the first finish because the fingerprint differs.
	// Iterations > 2 proves the finish was rejected into more work.
	if res.Iterations <= 2 {
		t.Errorf("Iterations = %d, want > 2 (finish was rejected and loop continued)", res.Iterations)
	}
	if !res.VerifyBaselineRed {
		t.Error("VerifyBaselineRed = false, want true")
	}
	// The model should have been called at least 3 times (write, finish, continue-feedback, finish).
	if len(ns.calls) < 3 {
		t.Errorf("model calls = %d, want >= 3", len(ns.calls))
	}
}

// TestVerifyContinueBaselineRescuePreserved proves that the single grace round
// rescues a fix-the-red-test task: the baseline is red because the verify
// command fails (here, `test -f calc.go` when calc.go does not exist), and a
// premature no-tool-call finish fingerprints identically to the baseline. With
// the grace round, the first identical failure is rejected via VerifyContinue
// (not terminal), the model gets the real red output, writes the missing file,
// and the verify passes on the second finish — the run is Answered.
func TestVerifyContinueBaselineRescuePreserved(t *testing.T) {
	// VerifyCmd checks for calc.go — missing at baseline, so the baseline is red.
	// Turn 1: premature finish (no tool calls), fingerprint identical to baseline
	//         → grace round: rejected via VerifyContinue, model sees red output.
	// Turn 2: write_file calc.go (actual fix work).
	// Turn 3: finish — verify now passes because calc.go exists → Answered.
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("done")},
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "package calc"})},
		{llm.Text("done — I created calc.go")},
	}}
	cfg := Config{
		Model:          ns,
		Sandbox:        sbWith(t, nil),
		Task:           "fix the build",
		VerifyCmd:      "test -f calc.go",
		VerifyContinue: true,
		MaxIterations:  10,
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %s, want Answered (grace round rescued the premature finish)", res.Outcome)
	}
	if !res.VerifyBaselineRed {
		t.Error("VerifyBaselineRed = false, want true (baseline was red because calc.go was missing)")
	}
	// The model should be called 3 times: premature finish (grace rejection),
	// write_file turn, then successful finish.
	if len(ns.calls) != 3 {
		t.Errorf("model calls = %d, want 3 (premature finish, write_file, successful finish)", len(ns.calls))
	}
}

// timedOutExecSandbox wraps a sandbox and returns a canned timeout result for
// Exec — used to simulate a verify command that exceeds its time bound.
type timedOutExecSandbox struct {
	sandbox.Sandbox
}

func (s *timedOutExecSandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	return &sandbox.Result{TimedOut: true, ExitCode: 124}, nil
}

// TestVerifyTimeoutAtFinish verifies that when VerifyCmd times out at the
// closing gate with VerifyContinue=true, the run ends Unverified at iteration 1
// (no continue loop), the reason says "inconclusive"/"timed out", and does NOT
// contain the "did not pass" wording.
func TestVerifyTimeoutAtFinish(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("done")},
	}}
	base := sbWith(t, nil)
	// The verify sandbox always times out, so the closing gate sees a timeout.
	cfg := Config{
		Model:          ns,
		Sandbox:        base,
		VerifySandbox:  &timedOutExecSandbox{Sandbox: base},
		Task:           "test task",
		VerifyCmd:      "go test ./...",
		VerifyContinue: true,
		MaxIterations:  10,
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Unverified {
		t.Errorf("Outcome = %s, want Unverified", res.Outcome)
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (no continue for timeout)", res.Iterations)
	}
	if !strings.Contains(res.Reason, "inconclusive") && !strings.Contains(res.Reason, "INCONCLUSIVE") {
		t.Errorf("Reason = %q, want it to say 'inconclusive' / 'INCONCLUSIVE'", res.Reason)
	}
	if strings.Contains(res.Reason, "did not pass") {
		t.Errorf("Reason = %q, must NOT contain 'did not pass' for a timeout", res.Reason)
	}
}

// TestBaselineTimeoutIsNotRed verifies that a timed-out baseline does NOT set
// VerifyBaselineRed, and with AbortOnRedBaseline=true the run still proceeds
// (does not refuse before the first model call).
func TestBaselineTimeoutIsNotRed(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("done")},
	}}
	base := sbWith(t, nil)
	cfg := Config{
		Model:              ns,
		Sandbox:            base,
		VerifySandbox:      &timedOutExecSandbox{Sandbox: base},
		Task:               "test task",
		VerifyCmd:          "go test ./...",
		AbortOnRedBaseline: true,
	}
	res, err := RunNative(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.VerifyBaselineRed {
		t.Error("VerifyBaselineRed = true, want false (timed-out baseline is no signal)")
	}
	// The run must have proceeded past the baseline — iterations > 0 proves
	// AbortOnRedBaseline did NOT fire.
	if res.Iterations == 0 {
		t.Error("Iterations = 0, want > 0 (AbortOnRedBaseline must not fire on timeout baseline)")
	}
	if len(ns.calls) == 0 {
		t.Error("model was never called — AbortOnRedBaseline falsely refused the run")
	}
}

// TestVerifyTimeoutResolution tests the verifyTimeout resolver directly.
func TestVerifyTimeoutResolution(t *testing.T) {
	// Explicit VerifyTimeout wins.
	if got := verifyTimeout(Config{VerifyTimeout: 42 * time.Second, RunTimeout: 30 * time.Second}, 30*time.Second); got != 42*time.Second {
		t.Errorf("explicit VerifyTimeout=42s: got %v, want 42s", got)
	}
	// VerifyTimeout=0, RunTimeout=30s -> floor of 5m.
	if got := verifyTimeout(Config{}, 30*time.Second); got != 5*time.Minute {
		t.Errorf("VerifyTimeout=0, RunTimeout=30s: got %v, want 5m", got)
	}
	// VerifyTimeout=0, RunTimeout=10m -> resolves to 10m (larger than floor).
	if got := verifyTimeout(Config{}, 10*time.Minute); got != 10*time.Minute {
		t.Errorf("VerifyTimeout=0, RunTimeout=10m: got %v, want 10m", got)
	}
	// VerifyTimeout=0, RunTimeout=0 (use defaultRunTimeout=30s) -> floor 5m.
	if got := verifyTimeout(Config{}, 0); got != 5*time.Minute {
		t.Errorf("VerifyTimeout=0, RunTimeout=0: got %v, want 5m", got)
	}
}
