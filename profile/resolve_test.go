package profile

import (
	"reflect"
	"strings"
	"testing"
)

func resolveProfile(t *testing.T, trust Trust, name string, ov Overrides) (Normalized, Trace, error) {
	t.Helper()
	e, err := ExecByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return Resolve(TrustPosture{Selected: trust, Posture: FloorFor(trust)}, e, ov)
}

func TestResolveCLIOverrideProvenanceAndCanonicality(t *testing.T) {
	maxIters := 60
	n, tr, err := resolveProfile(t, TrustedLocal, "coding-v2", Overrides{MaxIters: &maxIters})
	if err != nil {
		t.Fatal(err)
	}
	if n.MaxIters != maxIters {
		t.Fatalf("MaxIters = %d, want %d", n.MaxIters, maxIters)
	}
	if got := tr.Fields[string(MaxItersField)].Source; got != SourceCLI {
		t.Errorf("max_iters source = %q, want cli", got)
	}
	if tr.Canonical {
		t.Error("a profile-resolved CLI override must be non-canonical")
	}
	for _, f := range ProfileFields {
		if f == MaxItersField {
			continue
		}
		if got := tr.Fields[string(f)].Source; got != SourceProfile {
			t.Errorf("untouched %s source = %q, want profile", f, got)
		}
	}
}

func TestResolveSameValueOverrideIsNonCanonical(t *testing.T) {
	maxIters := 40
	_, tr, err := resolveProfile(t, TrustedLocal, "coding-v2", Overrides{MaxIters: &maxIters})
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.Fields[string(MaxItersField)].Source; got != SourceCLI {
		t.Errorf("max_iters source = %q, want cli for explicitly supplied value", got)
	}
	if tr.Canonical {
		t.Error("an explicit same-value override must be non-canonical")
	}
}

func TestResolveCLIOnlyDoesNotAffectCanonicality(t *testing.T) {
	_, tr, err := resolveProfile(t, TrustedLocal, "coding-v2", Overrides{CLIOnly: map[string]any{"verify_cmd": "go test ./..."}})
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := tr.Fields["verify_cmd"]
	if !ok || entry.Value != "go test ./..." || entry.Source != SourceCLI {
		t.Errorf("CLI-only trace = %+v, want cli verify_cmd", entry)
	}
	if !tr.Canonical {
		t.Error("CLI-only settings must not affect descriptor canonicality")
	}
}

func TestResolveTrustTightensDefaultWorktree(t *testing.T) {
	n, tr, err := resolveProfile(t, Container, "coding-v2", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if n.Worktree != "required" {
		t.Errorf("worktree = %q, want required", n.Worktree)
	}
	if got := tr.Fields[string(WorktreeField)].Source; got != SourceTrust {
		t.Errorf("worktree source = %q, want trust", got)
	}
	if !tr.Canonical {
		t.Error("trust tightening without an override must remain canonical")
	}
}

func TestResolveRejectsCLIWorktreeBelowTrustFloor(t *testing.T) {
	worktree := "off"
	_, _, err := resolveProfile(t, Container, "coding-v2", Overrides{Worktree: &worktree})
	if err == nil || !strings.Contains(err.Error(), "worktree") || !strings.Contains(err.Error(), "cli") {
		t.Fatalf("error = %v, want worktree CLI conflict", err)
	}
}

func TestResolveRejectsDemandedProfileWorktreeBelowTrustFloor(t *testing.T) {
	_, _, err := resolveProfile(t, Container, "eval-swe-v1", Overrides{})
	if err == nil || !strings.Contains(err.Error(), "worktree") || !strings.Contains(err.Error(), "profile demand") {
		t.Fatalf("error = %v, want worktree profile-demand conflict", err)
	}
}

func TestResolveRejectsProfileAboveSelectedTrust(t *testing.T) {
	e := Exec{Name: "test-container-required", RequiredTrust: Container}
	_, _, err := Resolve(TrustPosture{Selected: TrustedLocal, Posture: FloorFor(TrustedLocal)}, e, Overrides{})
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("error = %v, want required-trust sandbox dimension violation", err)
	}
}

func TestResolveWorktreeClassificationMatrix(t *testing.T) {
	// FloorFor worktree: trusted-local/reviewed-local=auto;
	// container/untrusted=required. Default profile values can tighten at the
	// latter floor; eval-swe-v1's explicit off demand instead conflicts there.
	cases := []struct {
		profile string
		trust   Trust
		want    string // profile, trust, or demand-error
	}{
		{"coding-v2", TrustedLocal, "profile"}, {"coding-v2", ReviewedLocal, "profile"}, {"coding-v2", Container, "trust"}, {"coding-v2", Untrusted, "trust"},
		{"interactive-v2", TrustedLocal, "profile"}, {"interactive-v2", ReviewedLocal, "profile"}, {"interactive-v2", Container, "trust"}, {"interactive-v2", Untrusted, "trust"},
		{"observe-v1", TrustedLocal, "profile"}, {"observe-v1", ReviewedLocal, "profile"}, {"observe-v1", Container, "trust"}, {"observe-v1", Untrusted, "trust"},
		{"eval-swe-v1", TrustedLocal, "profile"}, {"eval-swe-v1", ReviewedLocal, "profile"}, {"eval-swe-v1", Container, "demand-error"}, {"eval-swe-v1", Untrusted, "demand-error"},
	}
	for _, tc := range cases {
		t.Run(tc.profile+"/"+string(tc.trust), func(t *testing.T) {
			n, tr, err := resolveProfile(t, tc.trust, tc.profile, Overrides{})
			switch tc.want {
			case "demand-error":
				if err == nil || !strings.Contains(err.Error(), "profile demand") {
					t.Fatalf("error = %v, want profile demand conflict", err)
				}
			case "profile":
				if err != nil {
					t.Fatal(err)
				}
				e, _ := ExecByName(tc.profile)
				if n.Worktree != e.Worktree || tr.Fields[string(WorktreeField)].Source != SourceProfile {
					t.Errorf("worktree = %q (%s), want profile %q", n.Worktree, tr.Fields[string(WorktreeField)].Source, e.Worktree)
				}
			case "trust":
				if err != nil {
					t.Fatal(err)
				}
				if n.Worktree != "required" || tr.Fields[string(WorktreeField)].Source != SourceTrust {
					t.Errorf("worktree = %q (%s), want trust-required", n.Worktree, tr.Fields[string(WorktreeField)].Source)
				}
			}
		})
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	maxIters := 60
	input := Overrides{MaxIters: &maxIters, CLIOnly: map[string]any{"verify_cmd": "go test ./..."}}
	n1, tr1, err := resolveProfile(t, Container, "coding-v2", input)
	if err != nil {
		t.Fatal(err)
	}
	n2, tr2, err := resolveProfile(t, Container, "coding-v2", input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(n1, n2) || !reflect.DeepEqual(tr1, tr2) {
		t.Fatalf("identical Resolve inputs differed:\nfirst normalized=%+v trace=%+v\nsecond normalized=%+v trace=%+v", n1, tr1, n2, tr2)
	}
}
