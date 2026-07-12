package headless

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/profile"
)

var updateConfigGolden = flag.Bool("update", false, "update config golden files")

func TestConfigGolden(t *testing.T) {
	var got []agent.EffectiveConfig
	for _, trust := range []profile.Trust{profile.TrustedLocal, profile.ReviewedLocal} {
		for _, override := range []bool{false, true} {
			e, _ := profile.ExecByName("coding-v2")
			var ov profile.Overrides
			if override {
				n := 17
				ov.MaxIters = &n
			}
			r, tr, err := profile.Resolve(profile.TrustPosture{Selected: trust, Posture: profile.FloorFor(trust), Surface: "headless"}, e, ov)
			if err != nil {
				t.Fatal(err)
			}
			rounds := agent.DefaultReviewRounds
			if override {
				rounds = 3
			}
			cfg := buildBaseConfig(baseConfigInput{InvocationSurface: agent.InvocationSurfaceDriverRun, ExecProfile: e, RequireNetworkOff: profile.FloorFor(trust).RequiresNetworkOff(), RequiredTrust: string(tr.ProfileRequiredTrust), Canonical: tr.Canonical, MaxIterations: r.MaxIters, MaxTokens: r.MaxTokens, RunTimeout: r.RunTimeout, VerifyContinue: r.VerifyContinue, RequireDiff: r.RequireDiff, ReviewRounds: rounds, ReasoningEffort: r.Effort, BatchReads: r.BatchReads, BootContext: r.BootContext, ChurnNudgeRuns: r.ChurnNudgeRuns, FinishNudgeWindow: r.FinishNudge, AutoVerify: r.AutoVerify, AutoVerifySoft: r.AutoVerifySoft, StandingContext: r.StandingContext, NavSpiralWindow: r.NavSpiralWindow, AnswerNudgeWindow: r.AnswerNudgeWindow})
			spec, rt, _, err := cfg.Split()
			if err != nil {
				t.Fatalf("resolve run spec: %v", err)
			}
			got = append(got, agent.EffectiveConfigFromSpec(spec, rt))
		}
	}
	assertConfigGolden(t, "config_golden.json", got)
}

func assertConfigGolden(t *testing.T, name string, got any) {
	t.Helper()
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	path := filepath.Join("testdata", name)
	if *updateConfigGolden {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, b) {
		t.Fatalf("config golden differs; run go test ./... -run Golden -update\nwant:\n%s\ngot:\n%s", want, b)
	}
}
