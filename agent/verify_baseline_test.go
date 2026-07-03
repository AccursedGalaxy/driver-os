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
