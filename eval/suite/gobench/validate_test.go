package gobench

import (
	"strings"
	"testing"
	"time"
)

func redTest(pkg, name string) OracleTest {
	return OracleTest{TestID: TestID{Package: pkg, Name: name}}
}

func TestReduceRedAtBaseRun(t *testing.T) {
	tests := []OracleTest{redTest("./pkg", "TestA"), redTest("./pkg", "TestB")}
	cases := []struct {
		name        string
		statuses    map[string]goTestStatus
		wantRed     bool
		wantRan     bool
		wantPassing string
		wantMissing string
	}{
		{
			name: "all named tests fail is red",
			statuses: map[string]goTestStatus{
				"TestA": {ran: true, failed: true},
				"TestB": {ran: true, failed: true},
			},
			wantRed: true,
			wantRan: true,
		},
		{
			name: "partial pass is non-red and names passing test",
			statuses: map[string]goTestStatus{
				"TestA": {ran: true, failed: true},
				"TestB": {ran: true, passed: true},
			},
			wantRan:     true,
			wantPassing: "./pkg TestB",
		},
		{
			name: "all pass is non-red",
			statuses: map[string]goTestStatus{
				"TestA": {ran: true, passed: true},
				"TestB": {ran: true, passed: true},
			},
			wantRan:     true,
			wantPassing: "./pkg TestA",
		},
		{
			name: "missing named test is not run",
			statuses: map[string]goTestStatus{
				"TestA": {ran: true, failed: true},
			},
			wantMissing: "./pkg TestB",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := reduceRedAtBaseRun(tests, tt.statuses)
			if got.red != tt.wantRed || got.allNamedRan != tt.wantRan {
				t.Fatalf("reduceRedAtBaseRun() red=%v ran=%v, want red=%v ran=%v", got.red, got.allNamedRan, tt.wantRed, tt.wantRan)
			}
			if tt.wantPassing != "" && !strings.Contains(strings.Join(got.passing, ";"), tt.wantPassing) {
				t.Fatalf("passing detail %q does not contain %q", got.passing, tt.wantPassing)
			}
			if tt.wantMissing != "" && !strings.Contains(strings.Join(got.missing, ";"), tt.wantMissing) {
				t.Fatalf("missing detail %q does not contain %q", got.missing, tt.wantMissing)
			}
		})
	}
}

func TestClassifyRedAtBase(t *testing.T) {
	tests := []struct {
		name       string
		runs       []redRun
		wantOK     bool
		wantReason string
		wantDetail string
	}{
		{
			name:   "all red ok",
			runs:   []redRun{{allNamedRan: true, red: true}, {allNamedRan: true, red: true}},
			wantOK: true,
		},
		{
			name:       "partial pass no gate",
			runs:       []redRun{{allNamedRan: true, passing: []string{"./pkg TestB"}}, {allNamedRan: true, passing: []string{"./pkg TestB"}}},
			wantReason: "no-gate",
			wantDetail: "./pkg TestB",
		},
		{
			name:       "all green no gate",
			runs:       []redRun{{allNamedRan: true, passing: []string{"./pkg TestA", "./pkg TestB"}}, {allNamedRan: true, passing: []string{"./pkg TestA", "./pkg TestB"}}},
			wantReason: "no-gate",
			wantDetail: "./pkg TestA",
		},
		{
			name:       "missing test wins",
			runs:       []redRun{{allNamedRan: true, passing: []string{"./pkg TestA"}}, {allNamedRan: false, missing: []string{"./pkg TestB"}}},
			wantReason: "f2p-did-not-run",
			wantDetail: "./pkg TestB",
		},
		{
			name:       "mixed red non red flaky",
			runs:       []redRun{{allNamedRan: true, red: true}, {allNamedRan: true, passing: []string{"./pkg TestA"}}},
			wantReason: "flaky",
		},
		{
			name:   "single test red still ok",
			runs:   []redRun{{allNamedRan: true, red: true}},
			wantOK: true,
		},
		{
			name:       "single test pass still no gate",
			runs:       []redRun{{allNamedRan: true, passing: []string{"./pkg TestA"}}},
			wantReason: "no-gate",
			wantDetail: "./pkg TestA",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOK, gotReason, gotDetail := classifyRedAtBase(tt.runs)
			if gotOK != tt.wantOK || gotReason != tt.wantReason {
				t.Fatalf("classifyRedAtBase() = (%v, %q, %q), want (%v, %q, detail containing %q)", gotOK, gotReason, gotDetail, tt.wantOK, tt.wantReason, tt.wantDetail)
			}
			if tt.wantDetail != "" && !strings.Contains(gotDetail, tt.wantDetail) {
				t.Fatalf("detail %q does not contain %q", gotDetail, tt.wantDetail)
			}
		})
	}
}

func TestValidatorTestTimeoutPrecedence(t *testing.T) {
	inst := Instance{TestTimeout: "10m"}
	got, err := validatorTestTimeout(inst, ValidateOpts{TestTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got != 2*time.Minute {
		t.Fatalf("validatorTestTimeout() = %v, want 2m", got)
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
