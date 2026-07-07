package gobench

import "testing"

func TestClassifyRedAtBase(t *testing.T) {
	tests := []struct {
		name       string
		runs       []redRun
		wantOK     bool
		wantReason string
	}{
		{
			name:   "all red ok",
			runs:   []redRun{{allNamedRan: true}, {allNamedRan: true}},
			wantOK: true,
		},
		{
			name:       "all green no gate",
			runs:       []redRun{{allNamedRan: true, allPassed: true}, {allNamedRan: true, allPassed: true}},
			wantReason: "no-gate",
		},
		{
			name:       "missing test wins",
			runs:       []redRun{{allNamedRan: true, allPassed: true}, {allNamedRan: false}},
			wantReason: "f2p-did-not-run",
		},
		{
			name:       "mixed flaky",
			runs:       []redRun{{allNamedRan: true, allPassed: true}, {allNamedRan: true, allPassed: false}},
			wantReason: "flaky",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOK, gotReason := classifyRedAtBase(tt.runs)
			if gotOK != tt.wantOK || gotReason != tt.wantReason {
				t.Fatalf("classifyRedAtBase() = (%v, %q), want (%v, %q)", gotOK, gotReason, tt.wantOK, tt.wantReason)
			}
		})
	}
}

func TestClassifyGoldGreen(t *testing.T) {
	tests := []struct {
		name       string
		runs       []RunResult
		wantOK     bool
		wantReason string
	}{
		{
			name:   "all resolved ok",
			runs:   []RunResult{{Passed: true}, {Passed: true}},
			wantOK: true,
		},
		{
			name:       "all failed gold red",
			runs:       []RunResult{{Passed: false}, {Passed: false}},
			wantReason: "gold-red",
		},
		{
			name:       "mixed flaky",
			runs:       []RunResult{{Passed: true}, {Passed: false}},
			wantReason: "flaky",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOK, gotReason := classifyGoldGreen(tt.runs)
			if gotOK != tt.wantOK || gotReason != tt.wantReason {
				t.Fatalf("classifyGoldGreen() = (%v, %q), want (%v, %q)", gotOK, gotReason, tt.wantOK, tt.wantReason)
			}
		})
	}
}
