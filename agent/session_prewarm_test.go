package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// waitProbeDone blocks until the prewarm goroutine has published its
// resolution. The sandbox exec hook fires before probeAutoVerify returns, so
// tests that only wait on the exec would race the publication.
func waitProbeDone(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		s.autoVerifyProbe.Lock()
		done := s.autoVerifyProbe.done
		s.autoVerifyProbe.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("prewarm probe result was not published")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionPrewarmAutoVerifyDoesNotBlockAndAppliesOnce(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	sb := autoExecSandbox{Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}), exec: func(line string, _ time.Duration) *sandbox.Result {
		if line == "go build ./... && go test ./..." {
			<-release
			close(finished)
		}
		return &sandbox.Result{ExitCode: 0}
	}}
	spy := &noteSpy{}
	var got []Config
	s := newSessionT2(Config{Sandbox: sb, Root: ".", AutoVerify: true, Obs: spy}, func(_ context.Context, cfg Config) (*RunResult, error) {
		got = append(got, cfg)
		return &RunResult{}, nil
	})
	s.PrewarmAutoVerify(context.Background())

	started := time.Now()
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("first Send waited for probe: %s", elapsed)
	}
	if len(got) != 1 || got[0].VerifyCmd != "" || !got[0].autoVerifyResolved {
		t.Fatalf("first config = %+v, want unresolved unarmed turn", got[0])
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("prewarm probe did not finish")
	}
	waitProbeDone(t, s)
	if _, err := s.Send(context.Background(), "second"); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if len(got) != 2 || got[1].VerifyCmd != "go build ./... && go test ./..." || !got[1].AutoVerifySoft {
		t.Fatalf("second config did not arm auto verify: %+v", got[1])
	}
	var derived int
	for _, note := range spy.notes {
		if strings.Contains(note, "verify gate auto-derived") {
			derived++
		}
	}
	if derived != 1 {
		t.Fatalf("auto-derived notes = %d, want 1: %q", derived, spy.notes)
	}
}

func TestSessionPrewarmAutoVerifyAppliesRedBaseline(t *testing.T) {
	finished := make(chan struct{})
	sb := autoExecSandbox{Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}), exec: func(line string, _ time.Duration) *sandbox.Result {
		if line == "go build ./... && go test ./..." {
			close(finished)
			return &sandbox.Result{ExitCode: 1}
		}
		return &sandbox.Result{ExitCode: 0}
	}}
	spy := &noteSpy{}
	var got Config
	s := newSessionT2(Config{Sandbox: sb, Root: ".", AutoVerify: true, Obs: spy}, func(_ context.Context, cfg Config) (*RunResult, error) {
		got = cfg
		return &RunResult{}, nil
	})
	s.PrewarmAutoVerify(context.Background())
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("prewarm probe did not finish")
	}
	waitProbeDone(t, s)
	if _, err := s.Send(context.Background(), "turn"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.VerifyCmd != "" || !got.SkipVerifyBaseline || !got.autoVerifyResolved {
		t.Fatalf("red baseline config = %+v, want disarmed resolved config", got)
	}
	found := false
	for _, note := range spy.notes {
		found = found || strings.Contains(note, "already red on the untouched workspace")
	}
	if !found {
		t.Fatalf("missing red-baseline note: %q", spy.notes)
	}
}

func TestSessionWithoutPrewarmResolvesSynchronously(t *testing.T) {
	sb := autoExecSandbox{Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}), exec: func(string, time.Duration) *sandbox.Result {
		return &sandbox.Result{ExitCode: 0}
	}}
	var got Config
	s := newSessionT2(Config{Sandbox: sb, Root: ".", AutoVerify: true, Obs: &noteSpy{}}, func(ctx context.Context, cfg Config) (*RunResult, error) {
		resolveAutoVerifyOld(ctx, &cfg) // the loop's synchronous path, reached because no prewarm marked it resolved
		got = cfg
		return &RunResult{}, nil
	})
	if _, err := s.Send(context.Background(), "turn"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.VerifyCmd != "go build ./... && go test ./..." || !got.AutoVerifySoft {
		t.Fatalf("never-prewarmed session did not resolve synchronously: %+v", got)
	}
}
