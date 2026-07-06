package gobench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultTestTimeout = 10 * time.Minute

// Grade overlays held-out oracle tests onto an already-mutated checkout and
// returns the GoBench verdict. It is intentionally offline: checkoutDir and
// oracleDir must already exist locally.
func Grade(checkoutDir, oracleDir string, inst Instance) (Verdict, error) {
	v := Verdict{
		InstanceID:     inst.InstanceID,
		GraderVersion: GraderVersion,
	}

	timeout := defaultTestTimeout
	if inst.TestTimeout != "" {
		parsed, err := time.ParseDuration(inst.TestTimeout)
		if err != nil {
			v.GraderError = fmt.Sprintf("invalid test_timeout: %v", err)
			return v, err
		}
		timeout = parsed
	}

	// 1. Reset the agent's test edits before overlay. These commands are allowed
	// to fail when there is nothing to reset or clean.
	_ = runGit(context.Background(), checkoutDir, "checkout", "--", "*_test.go")
	_ = runGit(context.Background(), checkoutDir, "clean", "-fq", "--", "*_test.go")

	// 2. Overlay the held-out oracle files.
	for _, p := range inst.OracleFiles {
		if err := copyFile(filepath.Join(oracleDir, p), filepath.Join(checkoutDir, p)); err != nil {
			v.GraderError = fmt.Sprintf("overlay oracle failed: %s: %v", p, err)
			return v, err
		}
	}

	moduleDir := filepath.Join(checkoutDir, inst.ModuleDir)
	if inst.ModuleDir == "" {
		moduleDir = checkoutDir
	}

	// 3. Compile every package referenced by F2P or P2P. A failure here is a
	// grader-error signal, not a wrong-answer score.
	for _, pkg := range referencedPackages(inst) {
		res := runGoTest(checkoutDir, moduleDir, inst.Exec, timeout, testbuildArgs(inst.Exec, pkg))
		if res.err != nil {
			v.TestbuildOK = false
			v.Resolved = false
			v.GraderError = fmt.Sprintf("testbuild failed: %s: %s", pkg, head(nonEmpty(res.stderr, res.stdout), 1000))
			return v, nil
		}
	}
	v.TestbuildOK = true

	// 4. FAIL_TO_PASS: one -v exec per distinct package, and require proof that
	// every expected test actually emitted an === RUN line.
	f2pPass, ran, f2pReason := runFailToPass(checkoutDir, moduleDir, inst, timeout)
	v.F2PPass = f2pPass
	v.RanTests = ran
	if f2pReason != "" {
		v.GraderError = f2pReason
	}

	// 5. PASS_TO_PASS: package suites must stay green and are reported separately.
	p2pPass, p2pReason := runPassToPass(checkoutDir, moduleDir, inst, timeout)
	v.P2PPass = p2pPass
	if v.GraderError == "" && p2pReason != "" && strings.HasPrefix(p2pReason, "infra") {
		v.GraderError = p2pReason
	}

	// 6. Final resolve bit.
	v.Resolved = v.TestbuildOK && v.F2PPass && v.P2PPass
	return v, nil
}

type commandResult struct {
	stdout string
	stderr string
	err    error
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

func runFailToPass(checkoutDir, moduleDir string, inst Instance, timeout time.Duration) (bool, []TestID, string) {
	byPkg := map[string][]OracleTest{}
	for _, ot := range inst.FailToPass {
		byPkg[ot.Package] = append(byPkg[ot.Package], ot)
	}
	pkgs := sortedKeys(byPkg)
	allPass := true
	var ran []TestID
	var reasons []string
	for _, pkg := range pkgs {
		tests := byPkg[pkg]
		sort.Slice(tests, func(i, j int) bool { return testIDLess(tests[i].TestID, tests[j].TestID) })
		regex := combinedRunRegex(tests)
		res := runGoTest(checkoutDir, moduleDir, inst.Exec, timeout, f2pArgs(inst.Exec, regex, pkg))
		parsed := parseGoTestVerbose(res.stdout + res.stderr)
		for _, ot := range tests {
			name := fullTestName(ot.TestID)
			status := parsed[name]
			if status.ran {
				ran = append(ran, ot.TestID)
			}
			if !status.ran {
				allPass = false
				reasons = append(reasons, fmt.Sprintf("f2p test did not run: %s %s", pkg, name))
				continue
			}
			if !status.passed || status.failed {
				allPass = false
			}
		}
		if res.err != nil {
			allPass = false
		}
	}
	sort.Slice(ran, func(i, j int) bool { return testIDLess(ran[i], ran[j]) })
	if len(reasons) > 0 {
		return false, ran, strings.Join(reasons, "; ")
	}
	return allPass, ran, ""
}

func runPassToPass(checkoutDir, moduleDir string, inst Instance, timeout time.Duration) (bool, string) {
	pkgs := append([]string(nil), inst.PassToPass.Packages...)
	sort.Strings(pkgs)
	allPass := true
	for _, pkg := range pkgs {
		res := runGoTest(checkoutDir, moduleDir, inst.Exec, timeout, p2pArgs(inst.Exec, pkg))
		if res.err != nil {
			allPass = false
		}
	}
	return allPass, ""
}

func runGoTest(checkoutDir, moduleDir string, spec ExecSpec, timeout time.Duration, args []string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), spec.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("test command timed out after %s", timeout)
	}
	_ = checkoutDir // kept in the signature to make call sites explicit about repo root vs module dir.
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func baseArgv(spec ExecSpec) []string {
	if len(spec.Argv) == 0 {
		return []string{"go", "test"}
	}
	return append([]string(nil), spec.Argv...)
}

func tagsArgs(spec ExecSpec) []string {
	if len(spec.BuildTags) == 0 {
		return nil
	}
	tags := append([]string(nil), spec.BuildTags...)
	sort.Strings(tags)
	return []string{"-tags=" + strings.Join(tags, ",")}
}

func testbuildArgs(spec ExecSpec, pkg string) []string {
	args := baseArgv(spec)
	args = append(args, tagsArgs(spec)...)
	args = append(args, "-c", "-o", os.DevNull, pkg)
	return args
}

func f2pArgs(spec ExecSpec, regex, pkg string) []string {
	args := baseArgv(spec)
	args = append(args, tagsArgs(spec)...)
	args = append(args, "-count=1", "-v", "-run", regex, pkg)
	return args
}

func p2pArgs(spec ExecSpec, pkg string) []string {
	args := baseArgv(spec)
	args = append(args, tagsArgs(spec)...)
	args = append(args, "-count=1", pkg)
	return args
}

func referencedPackages(inst Instance) []string {
	set := map[string]bool{}
	for _, ot := range inst.FailToPass {
		set[ot.Package] = true
	}
	for _, pkg := range inst.PassToPass.Packages {
		set[pkg] = true
	}
	return sortedKeysBool(set)
}

func combinedRunRegex(tests []OracleTest) string {
	bodies := make([]string, 0, len(tests))
	for _, ot := range tests {
		bodies = append(bodies, stripAnchors(ot.RunRegex))
	}
	sort.Strings(bodies)
	return "^(" + strings.Join(bodies, "|") + ")$"
}

func stripAnchors(s string) string {
	if strings.HasPrefix(s, "^") {
		s = strings.TrimPrefix(s, "^")
	}
	if strings.HasSuffix(s, "$") {
		s = strings.TrimSuffix(s, "$")
	}
	return s
}

type goTestStatus struct {
	ran    bool
	passed bool
	failed bool
}

func parseGoTestVerbose(out string) map[string]goTestStatus {
	statuses := map[string]goTestStatus{}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "=== RUN   ") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "=== RUN   "))
			st := statuses[name]
			st.ran = true
			statuses[name] = st
			continue
		}
		if strings.HasPrefix(trimmed, "--- PASS: ") {
			name := testNameFromResultLine(strings.TrimPrefix(trimmed, "--- PASS: "))
			st := statuses[name]
			st.passed = true
			statuses[name] = st
			continue
		}
		if strings.HasPrefix(trimmed, "--- FAIL: ") {
			name := testNameFromResultLine(strings.TrimPrefix(trimmed, "--- FAIL: "))
			st := statuses[name]
			st.failed = true
			statuses[name] = st
		}
	}
	return statuses
}

func testNameFromResultLine(rest string) string {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func fullTestName(id TestID) string {
	if id.Subtest == "" {
		return id.Name
	}
	return id.Name + "/" + id.Subtest
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func sortedKeys(m map[string][]OracleTest) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysBool(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func testIDLess(a, b TestID) bool {
	if a.Package != b.Package {
		return a.Package < b.Package
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Subtest < b.Subtest
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func head(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
