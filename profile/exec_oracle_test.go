package profile

import (
	"reflect"
	"testing"
	"time"
)

// Oracle for PROFILES.md S3: versioned, immutable, compiled-in execution
// profiles. A built-in declares EVERY resolved field explicitly (no
// inheriting drifting package defaults); any resolution-affecting edit ships
// as a NEW versioned name; the registry is closed. The two built-ins encode
// TODAY'S per-surface defaults exactly — S3 changes where defaults LIVE, not
// what they are.
func TestExecBuiltinsEncodeTodaysSurfaceDefaults(t *testing.T) {
	coding, err := ExecByName("coding-v1")
	if err != nil {
		t.Fatal(err)
	}
	if coding.MaxIters != 40 || coding.MaxTokens != 8192 || coding.Effort != "low" ||
		coding.Worktree != "auto" || coding.StandingContext || coding.FinishNudge != 0 {
		t.Fatalf("coding-v1 must equal today's headless defaults (max-iters 40, max-tokens 8192, effort low, worktree auto, standing-context off, finish-nudge 0): %+v", coding)
	}
	tui, err := ExecByName("interactive-v1")
	if err != nil {
		t.Fatal(err)
	}
	if tui.MaxIters != 0 || tui.MaxTokens != 32768 || tui.Effort != "" ||
		tui.Worktree != "off" || !tui.StandingContext || tui.FinishNudge != 5 {
		t.Fatalf("interactive-v1 must equal today's TUI defaults (max-iters 0, max-tokens 32768, provider-default effort, worktree off, standing-context on, finish-nudge 5): %+v", tui)
	}
	for _, e := range []Exec{coding, tui} {
		if e.RunTimeout != 60*time.Second || !e.VerifyContinue || !e.Memory || !e.BatchReads || e.BootContext {
			t.Fatalf("%s: shared defaults (run-timeout 60s, verify-continue on, memory on, batch-reads on, boot-context off): %+v", e.Name, e)
		}
		if e.RequiredTrust != "" {
			t.Fatalf("%s: neither built-in declares a required trust in v1: %+v", e.Name, e)
		}
	}
}

func TestExecRegistryClosedAndHashed(t *testing.T) {
	if _, err := ExecByName("coding"); err == nil {
		t.Fatal("unversioned name must be rejected (names are versioned canonical ids)")
	}
	if _, err := ExecByName("nonexistent-v9"); err == nil {
		t.Fatal("unknown profile must error (closed registry)")
	}
	a, _ := ExecByName("coding-v1")
	b, _ := ExecByName("interactive-v1")
	if a.Hash() == "" || a.Hash() != a.Hash() {
		t.Fatal("profile content hash must be non-empty and stable")
	}
	if a.Hash() == b.Hash() {
		t.Fatal("different profiles must hash differently")
	}
}

// Generation two preserves every generation-one execution setting. Its only
// behavior change is the newly declared RequireDiff default for coding.
func TestV2ProfilesPreserveV1DescriptorFields(t *testing.T) {
	cases := []struct {
		v1, v2          string
		wantRequireDiff bool
	}{
		{"coding-v1", "coding-v2", true},
		{"interactive-v1", "interactive-v2", false},
	}
	for _, tc := range cases {
		v1, err := ExecByName(tc.v1)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := ExecByName(tc.v2)
		if err != nil {
			t.Fatal(err)
		}

		gotOriginal := struct {
			MaxIters, MaxTokens, FinishNudge int
			RunTimeout                       time.Duration
			Effort, Worktree                 string
			VerifyContinue, AutoVerify       bool
			Memory, BootContext              bool
			StandingContext, BatchReads      bool
			RequiredTrust                    Trust
		}{v2.MaxIters, v2.MaxTokens, v2.FinishNudge, v2.RunTimeout, v2.Effort, v2.Worktree, v2.VerifyContinue, v2.AutoVerify, v2.Memory, v2.BootContext, v2.StandingContext, v2.BatchReads, v2.RequiredTrust}
		wantOriginal := struct {
			MaxIters, MaxTokens, FinishNudge int
			RunTimeout                       time.Duration
			Effort, Worktree                 string
			VerifyContinue, AutoVerify       bool
			Memory, BootContext              bool
			StandingContext, BatchReads      bool
			RequiredTrust                    Trust
		}{v1.MaxIters, v1.MaxTokens, v1.FinishNudge, v1.RunTimeout, v1.Effort, v1.Worktree, v1.VerifyContinue, v1.AutoVerify, v1.Memory, v1.BootContext, v1.StandingContext, v1.BatchReads, v1.RequiredTrust}
		if !reflect.DeepEqual(gotOriginal, wantOriginal) {
			t.Errorf("%s original descriptor fields drifted from %s: got %+v, want %+v", tc.v2, tc.v1, gotOriginal, wantOriginal)
		}
		if v2.RequireDiff != tc.wantRequireDiff {
			t.Errorf("%s RequireDiff = %v, want %v", tc.v2, v2.RequireDiff, tc.wantRequireDiff)
		}
		v1Normalized, _, err := Resolve(TrustPosture{Selected: TrustedLocal, Posture: FloorFor(TrustedLocal)}, v1, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		gotAdded := struct {
			AutoVerifySoft    bool
			RequireDiff       bool
			ReadWindow        int
			ReadOutline       bool
			ChurnNudgeRuns    int
			NavSpiralWindow   int
			AnswerNudgeWindow int
		}{v2.AutoVerifySoft, v2.RequireDiff, v2.ReadWindow, v2.ReadOutline, v2.ChurnNudgeRuns, v2.NavSpiralWindow, v2.AnswerNudgeWindow}
		wantAdded := struct {
			AutoVerifySoft    bool
			RequireDiff       bool
			ReadWindow        int
			ReadOutline       bool
			ChurnNudgeRuns    int
			NavSpiralWindow   int
			AnswerNudgeWindow int
		}{v1Normalized.AutoVerifySoft, tc.wantRequireDiff, v1Normalized.ReadWindow, v1Normalized.ReadOutline, v1Normalized.ChurnNudgeRuns, v1Normalized.NavSpiralWindow, v1Normalized.AnswerNudgeWindow}
		if !reflect.DeepEqual(gotAdded, wantAdded) {
			t.Errorf("%s added fields = %+v, want v1 adapter values except RequireDiff=%v: %+v", tc.v2, gotAdded, tc.wantRequireDiff, wantAdded)
		}
	}
}
