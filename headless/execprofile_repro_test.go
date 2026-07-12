package headless

import (
	"flag"
	"sort"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/profile"
)

// Oracle for PROFILES.md S3 headless wiring: the execution profile supplies
// the flag DEFAULTS (so unset flags resolve to the named profile's values and
// explicitly-set flags override them — CLI > profile, no third layer), the
// -profile selection is pre-scanned from argv (a flag can't control other
// flags' defaults after registration), and the explicit-flag override list is
// what the config record carries as provenance.

func TestProfileFromArgsPreScan(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-task", "x"}, "coding-v2"},
		{[]string{"-profile", "interactive-v1", "-task", "x"}, "interactive-v1"},
		{[]string{"-profile=interactive-v1"}, "interactive-v1"},
	}
	for _, c := range cases {
		got, err := profileFromArgs(c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got.Name != c.want {
			t.Fatalf("profileFromArgs(%v) = %q, want %q", c.args, got.Name, c.want)
		}
	}
	if _, err := profileFromArgs([]string{"-profile", "bogus-v1"}); err == nil {
		t.Fatal("unknown profile must fail at startup (closed registry)")
	}
}

func TestRegisterFlagsWithProfileSetsDefaults(t *testing.T) {
	tui, _ := profile.ExecByName("interactive-v1")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := registerFlagsWithProfile(fs, tui)
	if err := fs.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	if *f.maxIters != 0 || *f.maxTokens != 32768 || *f.effort != "" {
		t.Fatalf("unset flags must resolve to the profile's values: max-iters=%d max-tokens=%d effort=%q", *f.maxIters, *f.maxTokens, *f.effort)
	}

	coding, _ := profile.ExecByName("coding-v1")
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	f2 := registerFlagsWithProfile(fs2, coding)
	if err := fs2.Parse([]string{"-max-iters", "7"}); err != nil {
		t.Fatal(err)
	}
	if *f2.maxIters != 7 || *f2.maxTokens != 8192 {
		t.Fatalf("explicit flag must override the profile, unset flags keep it: max-iters=%d max-tokens=%d", *f2.maxIters, *f2.maxTokens)
	}
	ov := cliOverrides(fs2)
	sort.Strings(ov)
	if strings.Join(ov, ",") != "max-iters" {
		t.Fatalf("cliOverrides must list exactly the explicitly-set flags; got %v", ov)
	}
}

// Require-diff follows the profile unless a CLI override is explicitly present.
func TestRequireDiffProfilePrecedence(t *testing.T) {
	posture := profile.TrustPosture{Selected: profile.TrustedLocal, Posture: profile.FloorFor(profile.TrustedLocal), Surface: "test"}
	for _, tc := range []struct {
		name, profile   string
		override        *bool
		want, canonical bool
	}{
		{"coding explicit false", "coding-v2", boolPtr(false), false, false},
		{"observe explicit true", "observe-v1", boolPtr(true), true, false},
		{"coding bare", "coding-v2", nil, true, true},
		{"observe bare", "observe-v1", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := profile.ExecByName(tc.profile)
			if err != nil {
				t.Fatal(err)
			}
			n, trace, err := profile.Resolve(posture, e, profile.Overrides{RequireDiff: tc.override})
			if err != nil {
				t.Fatal(err)
			}
			if n.RequireDiff != tc.want || trace.Canonical != tc.canonical {
				t.Fatalf("RequireDiff=%v canonical=%v, want %v/%v", n.RequireDiff, trace.Canonical, tc.want, tc.canonical)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

// callers), defaulting to coding-v1 — the headless surface's named profile.
func TestRegisterFlagsDefaultsToCodingV1(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := registerFlags(fs)
	if err := fs.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	if *f.maxIters != 40 || *f.effort != "low" {
		t.Fatalf("registerFlags must default to coding-v1: max-iters=%d effort=%q", *f.maxIters, *f.effort)
	}
}
