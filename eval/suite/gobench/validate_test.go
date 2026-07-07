package gobench

import (
	"context"
	"encoding/json"
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

func TestClassifyBuildFailure(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantInfra  bool
		wantDetail string
	}{
		{
			name:       "disk quota",
			output:     "go: creating work dir: mkdir /tmp/go-build: disk quota exceeded\n",
			wantInfra:  true,
			wantDetail: "disk quota exceeded",
		},
		{
			name:       "no space",
			output:     "write /tmp/cache: no space left on device\n",
			wantInfra:  true,
			wantDetail: "no space left on device",
		},
		{
			name:       "module fetch",
			output:     "go: github.com/acme/lib: cannot find module providing package github.com/acme/lib\n",
			wantInfra:  true,
			wantDetail: "cannot find module",
		},
		{
			name:       "network timeout",
			output:     "Get \"https://proxy.golang.org/mod\": dial tcp 10.0.0.1:443: i/o timeout\n",
			wantInfra:  true,
			wantDetail: "dial tcp",
		},
		{
			name:       "network dns",
			output:     "lookup proxy.golang.org: temporary failure in name resolution\n",
			wantInfra:  true,
			wantDetail: "temporary failure in name resolution",
		},
		{
			name:       "oom allocate",
			output:     "runtime: cannot allocate memory\n",
			wantInfra:  true,
			wantDetail: "cannot allocate memory",
		},
		{
			name:       "oom killed",
			output:     "compile: signal: killed\n",
			wantInfra:  true,
			wantDetail: "signal: killed",
		},
		{
			name:       "toolchain",
			output:     "go: invalid toolchain: go1.99.0\n",
			wantInfra:  true,
			wantDetail: "invalid toolchain",
		},
		{
			name:   "compile error stays code failure",
			output: "./foo.go:12:2: undefined: bar\n",
		},
		{
			name:   "empty output stays code failure",
			output: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyBuildFailure(tt.output)
			if (got != "") != tt.wantInfra {
				t.Fatalf("classifyBuildFailure() = %q, want infra=%v", got, tt.wantInfra)
			}
			if tt.wantDetail != "" && !strings.Contains(strings.ToLower(got), tt.wantDetail) {
				t.Fatalf("detail %q does not contain %q", got, tt.wantDetail)
			}
		})
	}
}

func TestClassifyRedAtBaseInfraError(t *testing.T) {
	ok, reason, detail := classifyRedAtBase([]redRun{{infraDetail: "compile: signal: killed"}})
	if ok || reason != "error" || !strings.Contains(detail, "signal: killed") {
		t.Fatalf("classifyRedAtBase() = (%v, %q, %q), want error with OOM detail", ok, reason, detail)
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

func TestClassifyGoldGreenInfraStaysOutOfGoldRed(t *testing.T) {
	detail := classifyBuildFailure("testbuild failed: ./pkg: go: toolchain not available")
	if !strings.Contains(detail, "toolchain not available") {
		t.Fatalf("classifyBuildFailure() = %q, want toolchain infra detail", detail)
	}
	ok, reason := classifyGoldGreen([]RunResult{{Passed: false}, {Passed: false}})
	if ok || reason != "gold-red" {
		t.Fatalf("classifyGoldGreen() = (%v, %q), want gold-red for genuine test failure", ok, reason)
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

func TestStage7AuthoredModeSkipsScrubAndPopulatesLeakScreen(t *testing.T) {
	old := runLeakScreen
	t.Cleanup(func() { runLeakScreen = old })
	var screened string
	runLeakScreen = func(ctx context.Context, inst Instance, checkoutDir string, threshold float64, screenedAt string) (LeakScreen, error) {
		screened = inst.ProblemStatement
		return LeakScreen{Method: "ngram-overlap", Passed: true, Threshold: threshold, ScreenedAt: screenedAt}, nil
	}

	inst := Instance{InstanceID: "authored", ProblemStatement: "human authored symptom"}
	rej, err := runStage7(context.Background(), &inst, "gold", ValidateOpts{Now: "2026-01-02T03:04:05Z", LeakThreshold: 0.12})
	if err != nil || rej != nil {
		t.Fatalf("runStage7 err=%v rej=%v", err, rej)
	}
	if screened != "human authored symptom" {
		t.Fatalf("screened statement=%q", screened)
	}
	if inst.Validation.LeakScreen.Method == "" {
		t.Fatalf("leak screen not populated")
	}
}

func TestStage7NoScrubStillPopulatesLeakScreen(t *testing.T) {
	old := runLeakScreen
	t.Cleanup(func() { runLeakScreen = old })
	called := false
	runLeakScreen = func(ctx context.Context, inst Instance, checkoutDir string, threshold float64, screenedAt string) (LeakScreen, error) {
		called = true
		if inst.ProblemStatement != "raw issue text" {
			t.Fatalf("statement was scrubbed: %q", inst.ProblemStatement)
		}
		return LeakScreen{Method: "ngram-overlap", Passed: true}, nil
	}

	inst := Instance{InstanceID: "noscrub", ProblemStatement: "raw issue text"}
	rej, err := runStage7(context.Background(), &inst, "gold", ValidateOpts{NoScrub: true, Scrubber: fakeScrubber{"scrubbed"}})
	if err != nil || rej != nil {
		t.Fatalf("runStage7 err=%v rej=%v", err, rej)
	}
	if !called || inst.Validation.LeakScreen.Method == "" {
		t.Fatalf("leak screen not populated; called=%v receipt=%+v", called, inst.Validation.LeakScreen)
	}
}

func TestStage7RejectsEmptyStatementBeforeLeakScreen(t *testing.T) {
	old := runLeakScreen
	t.Cleanup(func() { runLeakScreen = old })
	runLeakScreen = func(ctx context.Context, inst Instance, checkoutDir string, threshold float64, screenedAt string) (LeakScreen, error) {
		t.Fatalf("leak screen should not run for empty statement")
		return LeakScreen{}, nil
	}

	inst := Instance{InstanceID: "empty", ProblemStatement: " \t\n"}
	rej, err := runStage7(context.Background(), &inst, "gold", ValidateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if rej == nil || rej.Stage != "leak-screen" || rej.Reason != "empty-statement" {
		t.Fatalf("rejection=%+v", rej)
	}
}

func TestStage7ScrubbedPathScreensScrubbedStatement(t *testing.T) {
	old := runLeakScreen
	t.Cleanup(func() { runLeakScreen = old })
	var screened string
	runLeakScreen = func(ctx context.Context, inst Instance, checkoutDir string, threshold float64, screenedAt string) (LeakScreen, error) {
		screened = inst.ProblemStatement
		return LeakScreen{Method: "ngram-overlap", Passed: true}, nil
	}

	inst := Instance{InstanceID: "scrubbed", ProblemStatement: "raw fix hint"}
	rej, err := runStage7(context.Background(), &inst, "gold", ValidateOpts{Scrubber: fakeScrubber{"verbatim symptom"}})
	if err != nil || rej != nil {
		t.Fatalf("runStage7 err=%v rej=%v", err, rej)
	}
	if inst.ProblemStatement != "verbatim symptom" || screened != "verbatim symptom" {
		t.Fatalf("statement=%q screened=%q", inst.ProblemStatement, screened)
	}
	if inst.Validation.LeakScreen.Method == "" {
		t.Fatalf("leak screen not populated")
	}
}

func TestClassifyRedAtBaseWithGuardDemotionShape(t *testing.T) {
	tests := []OracleTest{
		{TestID: TestID{Package: "./cmd", Name: "TestBug"}, RunRegex: "^TestBug$"},
		{TestID: TestID{Package: "./cmd", Name: "TestGuard"}, RunRegex: "^TestGuard$"},
	}
	runs := []redRun{
		{allNamedRan: true, passing: []string{"./cmd TestGuard"}},
		{allNamedRan: true, passing: []string{"./cmd TestGuard"}},
	}
	ok, reason, detail := classifyRedAtBaseWithGuards(tests, runs)
	if !ok || reason != "" || detail != "" {
		t.Fatalf("classifyRedAtBaseWithGuards()=(%v,%q,%q), want ok", ok, reason, detail)
	}
	inst := Instance{FailToPass: tests, PassToPass: PassToPass{Packages: []string{"./other"}}}
	got, demotions := demoteGuardTests(inst, runs, "2026-07-07T00:00:00Z")
	if len(got.FailToPass) != 1 || got.FailToPass[0].Name != "TestBug" {
		t.Fatalf("retained F2P=%+v, want only TestBug", got.FailToPass)
	}
	if len(demotions) != 1 || demotions[0].Test.Name != "TestGuard" || demotions[0].Reason != "passes-at-base" {
		t.Fatalf("demotions=%+v", demotions)
	}
}

func TestClassifyRedAtBaseGuardFloorNoGate(t *testing.T) {
	tests := []OracleTest{redTest("./pkg", "TestA"), redTest("./pkg", "TestB")}
	runs := []redRun{
		{allNamedRan: true, passing: []string{"./pkg TestA", "./pkg TestB"}},
		{allNamedRan: true, passing: []string{"./pkg TestA", "./pkg TestB"}},
	}
	ok, reason, detail := classifyRedAtBaseWithGuards(tests, runs)
	if ok || reason != "no-gate" || !strings.Contains(detail, "./pkg TestA") || !strings.Contains(detail, "./pkg TestB") {
		t.Fatalf("classifyRedAtBaseWithGuards()=(%v,%q,%q), want no-gate naming tests", ok, reason, detail)
	}
}

func TestClassifyRedAtBaseMixedGuardIsFlaky(t *testing.T) {
	tests := []OracleTest{redTest("./pkg", "TestBug"), redTest("./pkg", "TestSometimes")}
	runs := []redRun{
		{allNamedRan: true, passing: []string{"./pkg TestSometimes"}},
		{allNamedRan: true, red: true},
	}
	ok, reason, _ := classifyRedAtBaseWithGuards(tests, runs)
	if ok || reason != "flaky" {
		t.Fatalf("classifyRedAtBaseWithGuards()=(%v,%q), want flaky", ok, reason)
	}
	inst := Instance{FailToPass: tests}
	got, demotions := demoteGuardTests(inst, runs, "stamp")
	if len(demotions) != 0 || len(got.FailToPass) != 2 {
		t.Fatalf("mixed result demoted=%+v retained=%+v", demotions, got.FailToPass)
	}
}

func TestDemoteGuardTestsExtendsP2PCoverage(t *testing.T) {
	tests := []OracleTest{
		{TestID: TestID{Package: "./bug", Name: "TestBug"}, RunRegex: "^TestBug$"},
		{TestID: TestID{Package: "./guard", Name: "TestGuard"}, RunRegex: "^TestGuard$"},
	}
	runs := []redRun{{allNamedRan: true, passing: []string{"./guard TestGuard"}}, {allNamedRan: true, passing: []string{"./guard TestGuard"}}}
	inst := Instance{FailToPass: tests, PassToPass: PassToPass{Packages: []string{"./bug"}}}
	got, _ := demoteGuardTests(inst, runs, "stamp")
	if !containsString(got.PassToPass.Packages, "./guard") {
		t.Fatalf("PASS_TO_PASS packages not extended: %+v", got.PassToPass.Packages)
	}

	inst = Instance{FailToPass: tests, PassToPass: PassToPass{Packages: []string{"./bug", "./guard"}}}
	got, _ = demoteGuardTests(inst, runs, "stamp")
	if len(got.PassToPass.Packages) != 2 {
		t.Fatalf("covered package should be unchanged, got %+v", got.PassToPass.Packages)
	}
}

func TestDemotionReceiptStampedFromOptsNow(t *testing.T) {
	tests := []OracleTest{redTest("./pkg", "TestBug"), redTest("./pkg", "TestGuard")}
	runs := []redRun{{allNamedRan: true, passing: []string{"./pkg TestGuard"}}, {allNamedRan: true, passing: []string{"./pkg TestGuard"}}}
	_, demotions := demoteGuardTests(Instance{FailToPass: tests}, runs, "2026-07-07T12:34:56Z")
	if len(demotions) != 1 || demotions[0].DemotedAt != "2026-07-07T12:34:56Z" || demotions[0].From != "FAIL_TO_PASS" || demotions[0].To != "PASS_TO_PASS" {
		t.Fatalf("demotion receipt=%+v", demotions)
	}
}

func TestValidationDemotionsJSONRoundTrip(t *testing.T) {
	v := Validation{Demotions: []Demotion{{Test: TestID{Package: "./pkg", Name: "TestGuard"}, From: "FAIL_TO_PASS", To: "PASS_TO_PASS", Reason: "passes-at-base", DemotedAt: "stamp"}}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "demotions") {
		t.Fatalf("json missing demotions: %s", b)
	}
	var got Validation
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Demotions) != 1 || got.Demotions[0].Test.Name != "TestGuard" || got.Demotions[0].Reason != "passes-at-base" {
		t.Fatalf("round trip=%+v", got.Demotions)
	}
	empty, err := json.Marshal(Validation{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "demotions") {
		t.Fatalf("empty demotions should be omitted: %s", empty)
	}
}
