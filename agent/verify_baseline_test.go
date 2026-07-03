package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
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
