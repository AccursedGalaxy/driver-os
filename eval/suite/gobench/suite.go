package gobench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/eval"
)

type Opts struct {
	InstancesDir string
	IDs          []string
	Count        int
	OraclesDir   string
	CacheDir     string
	TestTimeout  time.Duration
}

const taskPreamble = "You are at the root of a real Go repository checkout (%s). A working `go` " +
	"toolchain is available.\n\n" +
	"Below is an issue reported against this repository. Fix it by editing the repository's SOURCE code. " +
	"Do NOT modify or add tests — your fix is judged by held-back tests against the source alone, and " +
	"test-file edits are discarded. Reproduce the issue first if you can, make the fix surgical, re-check, " +
	"then answer with a one-paragraph summary of the root cause and your change.\n\n" +
	"--- ISSUE ---\n%s"

func Load(dir string) ([]Instance, error) {
	dir = resolveDir(dir)
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	instances := make([]Instance, 0, len(paths))
	for _, path := range paths {
		inst, err := LoadInstance(path)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

func DefaultInstancesDir() string { return "eval/suite/gobench/testdata/instances" }

func DefaultOraclesDir() string { return "docs/findings/harness-bench/swe-instances" }

func DefaultCacheDir() string { return filepath.Join(os.TempDir(), "gobench-cache") }

func resolveDir(dir string) string {
	if dir == "" || filepath.IsAbs(dir) {
		return dir
	}
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return dir
	}
	for {
		candidate := filepath.Join(wd, dir)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return dir
		}
		wd = parent
	}
}

func Cases(cfg agent.Config, opts Opts) ([]eval.Case, error) {
	instancesDir := opts.InstancesDir
	if instancesDir == "" {
		instancesDir = DefaultInstancesDir()
	}
	instances, err := Load(instancesDir)
	if err != nil {
		return nil, err
	}
	if len(opts.IDs) > 0 {
		instances, err = filterByID(instances, opts.IDs)
		if err != nil {
			return nil, err
		}
	} else if opts.Count > 0 && len(instances) > opts.Count {
		instances = instances[:opts.Count]
	}

	cases := make([]eval.Case, 0, len(instances))
	for _, inst := range instances {
		cases = append(cases, makeCase(cfg, inst, opts))
	}
	return cases, nil
}

func filterByID(instances []Instance, ids []string) ([]Instance, error) {
	byID := make(map[string]Instance, len(instances))
	for _, inst := range instances {
		byID[inst.InstanceID] = inst
		for _, alias := range inst.Aliases {
			if alias != "" {
				byID[alias] = inst
			}
		}
	}
	out := make([]Instance, 0, len(ids))
	for _, id := range ids {
		inst, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("gobench: instance %q not in the dataset", id)
		}
		out = append(out, inst)
	}
	return out, nil
}

func makeCase(cfg agent.Config, inst Instance, opts Opts) eval.Case {
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	oraclesDir := opts.OraclesDir
	if oraclesDir == "" {
		oraclesDir = DefaultOraclesDir()
	}
	if opts.TestTimeout > 0 {
		inst.TestTimeout = opts.TestTimeout.String()
	}
	return eval.Case{
		Name:         "gobench-" + inst.InstanceID,
		Task:         fmt.Sprintf(taskPreamble, inst.Repo, strings.TrimSpace(inst.ProblemStatement)),
		Protocol:     "tools",
		Config:       cfg,
		Fixture:      goBenchFixture{inst: inst, cacheDir: cacheDir},
		VCSWorkspace: true,
		LadderVerify: ladderVerifyCmd(inst),
		Oracle:       goBenchOracle{inst: inst, oraclesDir: oraclesDir},
	}
}

func ladderVerifyCmd(inst Instance) string {
	if len(inst.PassToPass.Packages) == 0 {
		return ""
	}
	args := append([]string(nil), inst.Exec.Argv...)
	if len(args) == 0 {
		args = []string{"go", "test"}
	}
	if len(inst.Exec.BuildTags) > 0 {
		tags := append([]string(nil), inst.Exec.BuildTags...)
		sort.Strings(tags)
		args = append(args, "-tags="+strings.Join(tags, ","))
	}
	args = append(args, "-count=1")
	args = append(args, inst.PassToPass.Packages...)

	cmd := strings.Join(shellQuoteArgs(args), " ")
	if inst.ModuleDir != "" {
		cmd = "cd " + shellQuote(inst.ModuleDir) + " && " + cmd
	}
	return cmd
}

type goBenchFixture struct {
	inst     Instance
	cacheDir string
}

func (f goBenchFixture) Materialize(dir string) error {
	return CheckoutBase(context.Background(), repoURL(f.inst.Repo), f.inst.BaseCommit, dir, f.cacheDir)
}

func (f goBenchFixture) Describe() string { return "gobench:" + f.inst.InstanceID }

func repoURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	if strings.Count(repo, "/") == 1 && !strings.HasSuffix(repo, ".git") {
		return "https://github.com/" + repo + ".git"
	}
	return repo
}

type goBenchOracle struct {
	inst       Instance
	oraclesDir string
}

func (o goBenchOracle) Grade(ctx context.Context, in eval.GradeInput) eval.Grade {
	shortname := o.inst.InstanceID
	if len(o.inst.Aliases) > 0 && o.inst.Aliases[0] != "" {
		shortname = o.inst.Aliases[0]
	}
	status, err := exec.CommandContext(ctx, "git", "-C", in.Root, "status", "--porcelain").Output()
	if err != nil {
		return eval.Grade{Pass: false, Detail: "git status: " + err.Error()}
	}
	if strings.TrimSpace(string(status)) == "" {
		return eval.Grade{NoAttempt: true, Detail: "unchanged working tree"}
	}

	verdict, err := Grade(in.Root, filepath.Join(o.oraclesDir, shortname, "oracle"), o.inst)
	if err != nil {
		return eval.Grade{Pass: false, Detail: err.Error()}
	}
	return eval.Grade{Pass: verdict.Resolved, Detail: verdictDetail(verdict)}
}

func verdictDetail(v Verdict) string {
	parts := []string{
		fmt.Sprintf("TestbuildOK=%t", v.TestbuildOK),
		fmt.Sprintf("F2PPass=%t", v.F2PPass),
		fmt.Sprintf("P2PPass=%t", v.P2PPass),
	}
	if v.GraderError != "" {
		parts = append(parts, "GraderError="+v.GraderError)
	}
	return strings.Join(parts, " ")
}

func shellQuoteArgs(args []string) []string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return quoted
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ',' || r == ':' || r == '+' || r == '@' || ('0' <= r && r <= '9') || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z'))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
