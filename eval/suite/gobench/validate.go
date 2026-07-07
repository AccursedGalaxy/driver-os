package gobench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ValidatorVersion = "gobench-validate/0.1.0"

type Rejection struct {
	InstanceID string `json:"instance_id"`
	Repo       string `json:"repo"`
	PR         int    `json:"pr"`
	Stage      string `json:"stage"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

type ValidateOpts struct {
	OracleDir    string
	CacheDir     string
	FlakeRuns    int
	Now          string
	BuildTimeout time.Duration
	TestTimeout  time.Duration

	Scrubber      Scrubber
	ScrubOutDir   string
	LeakThreshold float64
	NoScrub       bool
}

type redRun struct {
	allNamedRan bool
	red         bool
	passing     []string
	missing     []string
	infraDetail string
}

// Validate runs the in-package GoBench candidate validation pipeline.
func Validate(ctx context.Context, inst Instance, opts ValidateOpts) (Instance, *Rejection, error) {
	if inst.GoldCommit == "" {
		return inst, nil, fmt.Errorf("%s: gold_commit is empty", inst.InstanceID)
	}
	k := opts.FlakeRuns
	if k <= 0 {
		k = 5
	}
	buildTimeout := opts.BuildTimeout
	if buildTimeout == 0 {
		buildTimeout = 5 * time.Minute
	}
	testTimeout, err := validatorTestTimeout(inst, opts)
	if err != nil {
		return inst, nil, err
	}

	workDir, err := os.MkdirTemp("", "gobench-validate-*")
	if err != nil {
		return inst, nil, err
	}
	defer os.RemoveAll(workDir)

	baseDir := filepath.Join(workDir, "base")
	if err := CheckoutBase(ctx, repoURL(inst.Repo), inst.BaseCommit, baseDir, opts.CacheDir); err != nil {
		return inst, nil, err
	}
	if rej := validateEnvironment(inst, baseDir, buildTimeout); rej != nil {
		return inst, rej, nil
	}
	if rej := validateIsolation(inst, baseDir, opts.OracleDir, testTimeout); rej != nil {
		return inst, rej, nil
	}
	redResults, redRuns := runRedAtBase(inst, baseDir, testTimeout, k)
	if ok, reason, detail := classifyRedAtBase(redRuns); !ok {
		stage := "red-at-base"
		if reason == "error" {
			stage = "environment"
		}
		rej := rejection(inst, stage, reason, detail)
		return inst, rej, nil
	}

	goldDir := filepath.Join(workDir, "gold")
	if err := CheckoutBase(ctx, repoURL(inst.Repo), inst.GoldCommit, goldDir, opts.CacheDir); err != nil {
		return inst, nil, err
	}
	goldResults, infraDetail, err := runGoldGreen(inst, goldDir, opts.OracleDir, testTimeout, k)
	if err != nil {
		return inst, nil, err
	}
	if infraDetail != "" {
		return inst, rejection(inst, "environment", "error", infraDetail), nil
	}
	if ok, reason := classifyGoldGreen(goldResults); !ok {
		return inst, rejection(inst, "gold-green", reason, ""), nil
	}
	if slow := firstSlow(testTimeout, redResults, goldResults); slow != "" {
		return inst, rejection(inst, "timebox", "slow", slow), nil
	}

	out := inst
	if !opts.NoScrub {
		raw := out.ProblemStatement
		if err := scrubProblemStatement(ctx, &out, raw, opts.Scrubber, opts.ScrubOutDir); err != nil {
			return inst, nil, err
		}
		if rej := leakScreenInstance(ctx, &out, goldDir, opts.LeakThreshold, opts.Now); rej != nil {
			return out, rej, nil
		}
	}
	out.Validation.RedAtBaseRuns = redResults
	out.Validation.GoldGreenRuns = goldResults
	out.Validation.FlakeRuns = k
	out.Validation.ValidatorVer = ValidatorVersion
	out.ValidatedAt = opts.Now
	if err := out.Validate(); err != nil {
		return inst, nil, err
	}
	return out, nil, nil
}

func overlayOracle(checkoutDir, oracleDir string, oracleFiles []string) error {
	_ = runGit(context.Background(), checkoutDir, "checkout", "--", "*_test.go")
	_ = runGit(context.Background(), checkoutDir, "clean", "-fq", "--", "*_test.go")
	for _, p := range oracleFiles {
		if err := copyFile(filepath.Join(oracleDir, p), filepath.Join(checkoutDir, p)); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

func validatorTestTimeout(inst Instance, opts ValidateOpts) (time.Duration, error) {
	if opts.TestTimeout != 0 {
		return opts.TestTimeout, nil
	}
	if inst.TestTimeout == "" {
		return defaultTestTimeout, nil
	}
	return time.ParseDuration(inst.TestTimeout)
}

func validateEnvironment(inst Instance, checkoutDir string, timeout time.Duration) *Rejection {
	moduleDir := modulePath(checkoutDir, inst.ModuleDir)
	spec := withToolchain(inst.Exec, inst.GoVersion)
	res := runGoTest(checkoutDir, moduleDir, spec, timeout, []string{"go", "build", "./..."})
	if res.err != nil {
		detail := head(nonEmpty(res.stderr, res.stdout), 1000)
		if infraDetail := classifyBuildFailure(res.stderr + "\n" + res.stdout); infraDetail != "" {
			return rejection(inst, "environment", "error", infraDetail)
		}
		return rejection(inst, "environment", "broken-base", detail)
	}
	return nil
}

var buildFailureInfraSignatures = []string{
	"disk quota exceeded",
	"no space left on device",
	"cannot find module",
	"connection refused",
	"connection reset",
	"i/o timeout",
	"tls handshake timeout",
	"temporary failure in name resolution",
	"cannot allocate memory",
	"signal: killed",
	"invalid toolchain",
	"toolchain not available",
	"dial tcp",
}

func classifyBuildFailure(output string) string {
	lowerOutput := strings.ToLower(output)
	for _, sig := range buildFailureInfraSignatures {
		if idx := strings.Index(lowerOutput, sig); idx >= 0 {
			return firstMatchedLine(output, idx)
		}
	}
	return ""
}

func firstMatchedLine(output string, matchIdx int) string {
	start := strings.LastIndex(output[:matchIdx], "\n") + 1
	end := strings.Index(output[matchIdx:], "\n")
	if end < 0 {
		end = len(output)
	} else {
		end = matchIdx + end
	}
	return strings.TrimSpace(output[start:end])
}

func validateIsolation(inst Instance, checkoutDir, oracleDir string, timeout time.Duration) *Rejection {
	if err := overlayOracle(checkoutDir, oracleDir, inst.OracleFiles); err != nil {
		return rejection(inst, "isolation", "base-testbuild-fail", err.Error())
	}
	moduleDir := modulePath(checkoutDir, inst.ModuleDir)
	spec := withToolchain(inst.Exec, inst.GoVersion)
	for _, pkg := range referencedPackages(inst) {
		res := runGoTest(checkoutDir, moduleDir, spec, timeout, testbuildArgs(spec, pkg))
		if res.err == nil {
			continue
		}
		detail := head(nonEmpty(res.stderr, res.stdout), 1000)
		reason := "base-testbuild-fail"
		lower := strings.ToLower(detail)
		if strings.Contains(lower, "redeclared") || strings.Contains(lower, "already declared") {
			reason = "overlay-collision"
		}
		return rejection(inst, "isolation", reason, detail)
	}
	return nil
}

func runRedAtBase(inst Instance, checkoutDir string, timeout time.Duration, k int) ([]RunResult, []redRun) {
	moduleDir := modulePath(checkoutDir, inst.ModuleDir)
	spec := withToolchain(inst.Exec, inst.GoVersion)
	byPkg := map[string][]OracleTest{}
	for _, ot := range inst.FailToPass {
		byPkg[ot.Package] = append(byPkg[ot.Package], ot)
	}
	pkgs := sortedKeys(byPkg)
	results := make([]RunResult, 0, k)
	classRuns := make([]redRun, 0, k)
	for i := 0; i < k; i++ {
		start := time.Now()
		allRan, allPassed := true, true
		var ran []TestID
		var runParts []redRun
		for _, pkg := range pkgs {
			tests := byPkg[pkg]
			sort.Slice(tests, func(i, j int) bool { return testIDLess(tests[i].TestID, tests[j].TestID) })
			res := runGoTest(checkoutDir, moduleDir, spec, timeout, f2pArgs(spec, combinedRunRegex(tests), pkg))
			parsed := parseGoTestVerbose(res.stdout + res.stderr)
			rr := reduceRedAtBaseRun(tests, parsed)
			runParts = append(runParts, rr)
			for _, ot := range tests {
				st := parsed[fullTestName(ot.TestID)]
				if st.ran {
					ran = append(ran, ot.TestID)
				} else {
					allRan = false
					allPassed = false
				}
				if !st.passed || st.failed {
					allPassed = false
				}
			}
			if res.err != nil {
				allPassed = false
				if infraDetail := classifyBuildFailure(res.stderr + "\n" + res.stdout); infraDetail != "" && rr.infraDetail == "" {
					rr.infraDetail = infraDetail
				}
			}
		}
		results = append(results, RunResult{Passed: allRan && allPassed, RanTests: ran, DurationS: time.Since(start).Seconds()})
		classRuns = append(classRuns, mergeRedRuns(runParts))
	}
	return results, classRuns
}

func reduceRedAtBaseRun(tests []OracleTest, parsed map[string]goTestStatus) redRun {
	r := redRun{allNamedRan: true, red: true}
	for _, ot := range tests {
		name := fullTestName(ot.TestID)
		st := parsed[name]
		label := fmt.Sprintf("%s %s", ot.Package, name)
		if !st.ran {
			r.allNamedRan = false
			r.red = false
			r.missing = append(r.missing, label)
			continue
		}
		if !st.failed {
			r.red = false
			r.passing = append(r.passing, label)
		}
	}
	return r
}

func mergeRedRuns(parts []redRun) redRun {
	r := redRun{allNamedRan: true, red: true}
	for _, part := range parts {
		if !part.allNamedRan {
			r.allNamedRan = false
		}
		if !part.red {
			r.red = false
		}
		r.passing = append(r.passing, part.passing...)
		r.missing = append(r.missing, part.missing...)
		if r.infraDetail == "" {
			r.infraDetail = part.infraDetail
		}
	}
	sort.Strings(r.passing)
	sort.Strings(r.missing)
	return r
}

func runGoldGreen(inst Instance, checkoutDir, oracleDir string, timeout time.Duration, k int) ([]RunResult, string, error) {
	results := make([]RunResult, 0, k)
	for i := 0; i < k; i++ {
		start := time.Now()
		v, err := GradeWithTimeout(checkoutDir, oracleDir, inst, timeout)
		if err != nil {
			return nil, "", err
		}
		if infraDetail := classifyBuildFailure(v.GraderError); infraDetail != "" {
			return results, infraDetail, nil
		}
		results = append(results, RunResult{Passed: v.Resolved, RanTests: v.RanTests, DurationS: time.Since(start).Seconds()})
	}
	return results, "", nil
}

func classifyRedAtBase(runs []redRun) (ok bool, reason string, detail string) {
	allRed, allNonRed := true, true
	var passing []string
	for _, r := range runs {
		if r.infraDetail != "" {
			return false, "error", r.infraDetail
		}
		if !r.allNamedRan {
			return false, "f2p-did-not-run", strings.Join(r.missing, "; ")
		}
		if r.red {
			allNonRed = false
		} else {
			allRed = false
			passing = append(passing, r.passing...)
		}
	}
	if allRed {
		return true, "", ""
	}
	if allNonRed {
		sort.Strings(passing)
		return false, "no-gate", strings.Join(uniqueStrings(passing), "; ")
	}
	return false, "flaky", ""
}

func uniqueStrings(vals []string) []string {
	if len(vals) == 0 {
		return nil
	}
	out := vals[:0]
	var prev string
	for i, v := range vals {
		if i == 0 || v != prev {
			out = append(out, v)
			prev = v
		}
	}
	return out
}

func classifyGoldGreen(runs []RunResult) (ok bool, reason string) {
	allPassed, allFailed := true, true
	for _, r := range runs {
		if r.Passed {
			allFailed = false
		} else {
			allPassed = false
		}
	}
	if allPassed {
		return true, ""
	}
	if allFailed {
		return false, "gold-red"
	}
	return false, "flaky"
}

func firstSlow(timeout time.Duration, groups ...[]RunResult) string {
	limit := timeout.Seconds()
	for _, runs := range groups {
		for _, r := range runs {
			if r.DurationS > limit {
				return fmt.Sprintf("run took %.3fs > %.3fs", r.DurationS, limit)
			}
		}
	}
	return ""
}

func modulePath(checkoutDir, moduleDir string) string {
	if moduleDir == "" {
		return checkoutDir
	}
	return filepath.Join(checkoutDir, moduleDir)
}

func withToolchain(spec ExecSpec, goVersion string) ExecSpec {
	out := spec
	if goVersion != "" {
		out.Env = append(ToolchainEnv(goVersion), out.Env...)
	}
	return out
}

func rejection(inst Instance, stage, reason, detail string) *Rejection {
	return &Rejection{InstanceID: inst.InstanceID, Repo: inst.Repo, PR: inst.PRNumber, Stage: stage, Reason: reason, Detail: detail}
}
