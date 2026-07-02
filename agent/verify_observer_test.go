package agent

import (
	"context"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// VerifyObserver is the TUI's verify-chip seam: a front-end that shows the
// closing gate's live green/red cannot parse Note prose (a PASSING
// verification emits no Note at all), so every VerifyCmd execution must reach
// an opted-in observer as a typed result. These pin the two emission paths —
// the finish gate (verifyTermination) and the kill/cap upgrade
// (upgradeIfVerified) — plus the user-cancel skip, where nothing was measured
// so nothing must be reported.

// verifySpy records every VerifyResult; the embedded nopObserver absorbs the
// rest of the Observer surface.
type verifySpy struct {
	nopObserver
	cmds []string
	oks  []bool
}

func (s *verifySpy) VerifyResult(cmd string, ok bool) {
	s.cmds = append(s.cmds, cmd)
	s.oks = append(s.oks, ok)
}

func TestVerifyObserverSeesFinishGateResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmd     string
		wantOK  bool
		outcome Outcome
	}{
		{"pass", "true", true, Answered},
		{"fail", "false", false, Unverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &verifySpy{}
			res, err := RunNative(context.Background(), Config{
				Model:     &nativeScript{turns: [][]llm.ContentPart{{llm.Text("all done")}}},
				Sandbox:   sbWith(t, map[string]string{"go.mod": "module x\n"}),
				Task:      "test task",
				VerifyCmd: tc.cmd,
				Obs:       spy,
			})
			if err != nil {
				t.Fatalf("RunNative: %v", err)
			}
			if res.Outcome != tc.outcome {
				t.Fatalf("Outcome = %s, want %s", res.Outcome, tc.outcome)
			}
			if len(spy.oks) != 1 || spy.oks[0] != tc.wantOK || spy.cmds[0] != tc.cmd {
				t.Fatalf("observer saw cmds=%v oks=%v; want one %q result with ok=%v", spy.cmds, spy.oks, tc.cmd, tc.wantOK)
			}
		})
	}
}

func TestVerifyObserverSeesUpgradeCheck(t *testing.T) {
	spy := &verifySpy{}
	cfg := Config{Sandbox: sbWith(t, nil), VerifyCmd: "true", Obs: spy}
	res := upgradeIfVerified(context.Background(), cfg, &RunResult{Outcome: HitCap}, time.Second)
	if res.Outcome != Answered {
		t.Fatalf("upgrade did not happen: %s", res.Outcome)
	}
	if len(spy.oks) != 1 || !spy.oks[0] {
		t.Fatalf("observer saw oks=%v; want one true result from the upgrade check", spy.oks)
	}
}

func TestVerifyObserverSilentOnUserCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // user cancel — the check never runs, so nothing must be reported.
	spy := &verifySpy{}
	cfg := Config{Sandbox: sbWith(t, nil), VerifyCmd: "true", Obs: spy}
	upgradeIfVerified(ctx, cfg, &RunResult{Outcome: HitCap}, time.Second)
	verifyTermination(ctx, cfg, false, time.Second)
	if len(spy.oks) != 0 {
		t.Fatalf("observer saw %v on user cancel; a skipped check must emit nothing", spy.oks)
	}
}

// SetVerifyCmd must govern the NEXT Send — the /verify seam: arming a gate on
// a live conversation, then disarming it with "".
func TestSessionSetVerifyCmd(t *testing.T) {
	var saw []string
	loop := func(_ context.Context, cfg Config) (*RunResult, error) {
		saw = append(saw, cfg.VerifyCmd)
		return &RunResult{Outcome: Answered, Answer: "ok"}, nil
	}
	s := NewSession(Config{Model: swapProvider{id: "m"}}, loop)
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	s.SetVerifyCmd("go test ./...")
	if got := s.VerifyCmd(); got != "go test ./..." {
		t.Fatalf("VerifyCmd() = %q after SetVerifyCmd", got)
	}
	if _, err := s.Send(context.Background(), "second"); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	s.SetVerifyCmd("")
	if _, err := s.Send(context.Background(), "third"); err != nil {
		t.Fatalf("Send 3: %v", err)
	}
	want := []string{"", "go test ./...", ""}
	for i := range want {
		if saw[i] != want[i] {
			t.Fatalf("loop saw VerifyCmd %q on turn %d, want %q", saw[i], i+1, want[i])
		}
	}
}

// reviewerStub is the minimal Reviewer a SetReviewer test needs.
type reviewerStub struct{ id string }

func (r reviewerStub) Review(context.Context, ReviewInput) (*ReviewVerdict, error) {
	return &ReviewVerdict{}, nil
}

// SetReviewer must govern the NEXT Send — the /review seam: arming the review
// gate on a live conversation, then disarming it with nil.
func TestSessionSetReviewer(t *testing.T) {
	var saw []Reviewer
	loop := func(_ context.Context, cfg Config) (*RunResult, error) {
		saw = append(saw, cfg.Reviewer)
		return &RunResult{Outcome: Answered, Answer: "ok"}, nil
	}
	s := NewSession(Config{Model: swapProvider{id: "m"}}, loop)
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	s.SetReviewer(reviewerStub{id: "gpt"})
	if got, ok := s.Reviewer().(reviewerStub); !ok || got.id != "gpt" {
		t.Fatalf("Reviewer() = %#v after SetReviewer", s.Reviewer())
	}
	if _, err := s.Send(context.Background(), "second"); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	s.SetReviewer(nil)
	if _, err := s.Send(context.Background(), "third"); err != nil {
		t.Fatalf("Send 3: %v", err)
	}
	if saw[0] != nil || saw[1] == nil || saw[2] != nil {
		t.Fatalf("loop saw reviewers %v; want [nil non-nil nil]", saw)
	}
}
