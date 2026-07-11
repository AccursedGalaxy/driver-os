package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestInfraFaultSignature(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"disk quota", "RUN failed\nDisk quota exceeded", "disk quota exceeded"},
		{"no space", "write: no space left on device", "no space left on device"},
		{"memory", "fork/exec: cannot allocate memory", "cannot allocate memory"},
		{"files", "open x: too many open files", "too many open files"},
		{"generic killed", "signal: killed", ""},
		{"quoted in test failure", "--- FAIL: TestMessage\n    message says disk quota exceeded\nFAIL", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := infraFaultSignature(tc.out); got != tc.want {
				t.Fatalf("infraFaultSignature() = %q, want %q", got, tc.want)
			}
			if got := isInfraFault(tc.out); got != (tc.want != "") {
				t.Fatalf("isInfraFault() = %v, want %v", got, tc.want != "")
			}
		})
	}
}

func TestVerifyTerminationInfraFaultIsInconclusive(t *testing.T) {
	cfg := Config{Sandbox: sbWith(t, nil), VerifyCmd: "sh -c 'echo disk quota exceeded; exit 1'"}
	reason, out := verifyTerminationT(context.Background(), cfg, false, time.Second)
	if !strings.Contains(reason, "INCONCLUSIVE") || !strings.Contains(reason, "environment fault") {
		t.Fatalf("reason = %q, want inconclusive environment fault", reason)
	}
	if !isInfraFault(out) {
		t.Fatalf("verifyOut = %q, want infra", out)
	}

	cfg.VerifyCmd = "sh -c 'echo FAIL; exit 1'"
	reason, _ = verifyTerminationT(context.Background(), cfg, false, time.Second)
	if !strings.Contains(reason, "did not pass") {
		t.Fatalf("reason = %q, want normal red", reason)
	}
}

func TestStagnationIgnoresInfraFault(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 8, DefaultTerminationPolicy(), nil)
	// Real observation format: isRunFailure keys on the "exit N " prefix.
	obs := "exit 1 (5ms)\nstdout:\ndisk quota exceeded"
	for i := 0; i < maxStagnant+1; i++ {
		kill, count := tr.observeRun(obs)
		if kill || count != 0 || tr.stagnant != 0 || tr.lastRunFailed {
			t.Fatalf("observeRun infra #%d = kill %v count %d stagnant %d failed %v, want ignored", i, kill, count, tr.stagnant, tr.lastRunFailed)
		}
	}
}

// An infra blip between identical real failures must not launder the
// stagnation streak back to zero — it is no-signal, not progress.
func TestStagnationSurvivesInfraBlip(t *testing.T) {
	tr := newTurnTrackerT(Config{}, 8, DefaultTerminationPolicy(), nil)
	fail := "exit 1 (5ms)\nstdout:\n--- FAIL: TestX\nFAIL"
	tr.observeRun(fail)
	if _, count := tr.observeRun(fail); count != 2 {
		t.Fatalf("streak before blip = %d, want 2", count)
	}
	if _, count := tr.observeRun("exit 1 (5ms)\nstdout:\ndisk quota exceeded"); count != 2 {
		t.Fatalf("streak after infra blip = %d, want 2 (untouched)", count)
	}
	if tr.lastRunFailed {
		t.Fatal("infra blip should not count as a failed run")
	}
	if _, count := tr.observeRun(fail); count != 3 {
		t.Fatalf("streak after blip+same failure = %d, want 3 (continued)", count)
	}
}

func TestUpgradeIfVerifiedRetriesInfraOnce(t *testing.T) {
	sb := sbWith(t, map[string]string{"changed.txt": "x"})
	cfg := Config{Sandbox: sb, VerifySandbox: sb, VerifyCmd: "sh -c 'test -f marker && exit 0; touch marker; echo disk quota exceeded; exit 1'", SkipVerifyBaseline: true}
	g := mustNewGates(t, context.Background(), cfg, time.Second)
	res := g.upgradeIfVerified(context.Background(), &RunResult{Outcome: HitCap, Reason: "hit cap"})
	if res.Outcome != Answered {
		t.Fatalf("outcome = %s, want answered; reason %q", res.Outcome, res.Reason)
	}
	if g.verifyInfraSignature != "" {
		t.Fatalf("verifyInfraSignature = %q, want cleared after green retry", g.verifyInfraSignature)
	}
}

func TestUpgradeIfVerifiedRecordsRepeatedInfra(t *testing.T) {
	sb := sbWith(t, map[string]string{"changed.txt": "x"})
	cfg := Config{Sandbox: sb, VerifySandbox: sb, VerifyCmd: "sh -c 'echo disk quota exceeded; exit 1'", SkipVerifyBaseline: true}
	g := mustNewGates(t, context.Background(), cfg, time.Second)
	res := g.upgradeIfVerified(context.Background(), &RunResult{Outcome: HitCap, Reason: "hit cap"})
	if res.Outcome != HitCap {
		t.Fatalf("outcome = %s, want hit_cap", res.Outcome)
	}
	if g.verifyInfraSignature != "disk quota exceeded" || !res.VerifyInfra || res.VerifyInfraSignature != "disk quota exceeded" || !strings.Contains(res.Reason, "environment fault") {
		t.Fatalf("signature/result/reason = %q / %v %q / %q, want infra evidence", g.verifyInfraSignature, res.VerifyInfra, res.VerifyInfraSignature, res.Reason)
	}
}

func TestVerifyCompletionInfraSurfacedAfterGate(t *testing.T) {
	cfg := Config{Sandbox: sbWith(t, nil), VerifyCmd: "sh -c 'echo disk quota exceeded; exit 1'", SkipVerifyBaseline: true}
	g := mustNewGates(t, context.Background(), cfg, time.Second)
	outcome, reason, noContinue := g.verifyCompletion(context.Background(), false)
	if outcome != Unverified || !noContinue || !strings.Contains(reason, "environment fault") {
		t.Fatalf("verifyCompletion = (%s, %q, %v), want inconclusive infra unverified/noContinue", outcome, reason, noContinue)
	}
	res := &RunResult{}
	g.applyVerifyInfra(res)
	if !res.VerifyInfra || res.VerifyInfraSignature != "disk quota exceeded" {
		t.Fatalf("VerifyInfra = %v %q, want disk quota evidence", res.VerifyInfra, res.VerifyInfraSignature)
	}
}
