package headless

// Honest default posture (harness audit 2026-07-07, open item 2): -verify-cmd
// hands the solver the green gate, so leaving the test fence off by default is
// the gameable corner. When -verify-cmd is armed and the caller expressed no
// explicit fencing intent, the canonical test fence auto-arms; opting out is
// explicit (-test-fence=off|none, an explicit fence, or a -diff-scope, which
// declares the writable set and may deliberately include tests).

import (
	"strings"
	"testing"
)

func TestResolveTestFence(t *testing.T) {
	cases := []struct {
		name      string
		flagValue string
		verifyCmd string
		diffScope []string
		want      string // "auto" = canonical fence; "off" = nil; else exact globs
	}{
		{"auto-arms with verify-cmd", "", "go test ./...", nil, "auto"},
		{"off without verify-cmd", "", "", nil, "off"},
		{"explicit off opts out", "off", "go test ./...", nil, "off"},
		{"explicit none opts out", "none", "go test ./...", nil, "off"},
		{"diff-scope suppresses auto-fence", "", "go test ./...", []string{"pkg/**"}, "off"},
		{"explicit fence wins", "ci/**,*.lock", "go test ./...", nil, "ci/**,*.lock"},
		{"explicit fence without verify-cmd still arms", "*_test.go", "", nil, "*_test.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTestFence(tc.flagValue, tc.verifyCmd, tc.diffScope)
			switch tc.want {
			case "off":
				if len(got) != 0 {
					t.Fatalf("resolveTestFence(%q, %q, %v) = %v, want no fence", tc.flagValue, tc.verifyCmd, tc.diffScope, got)
				}
			case "auto":
				joined := strings.Join(got, ",")
				// The canonical fence must cover at least the measured domains:
				// Go tests + testdata, python tests. Exact extra globs are free
				// to grow — these are the load-bearing ones.
				for _, must := range []string{"*_test.go", "testdata/**", "test_*.py", "tests/**"} {
					if !strings.Contains(joined, must) {
						t.Fatalf("auto-armed fence %q is missing canonical glob %q", joined, must)
					}
				}
			default:
				if strings.Join(got, ",") != tc.want {
					t.Fatalf("resolveTestFence(%q, ...) = %v, want %q verbatim", tc.flagValue, got, tc.want)
				}
			}
		})
	}
}
