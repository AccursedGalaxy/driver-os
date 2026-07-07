package gobench

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// PullRequest is the small PR shape the miner needs after the gh crawl stage.
type PullRequest struct {
	Number        int
	MergedAt      string
	Additions     int
	Deletions     int
	ClosingIssues []int
	Files         []PRFile
}

// PRFile is one changed file in a mined pull request.
type PRFile struct {
	Path      string
	Additions int
	Deletions int
}

// RepoConfig is one entry from repos.yaml.
type RepoConfig struct {
	Repo          string `yaml:"repo"`
	License       string `yaml:"license"`
	TestFramework string `yaml:"test_framework"`
	Cgo           string `yaml:"cgo"`
	Status        string `yaml:"status"`
	Notes         string `yaml:"notes"`
}

// RejectRecord records the single reason a crawled PR was dropped.
type RejectRecord struct {
	Repo   string `json:"repo"`
	PR     int    `json:"pr"`
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
}

// LoadRepos parses the top-level repos: list used by the miner.
func LoadRepos(path string) ([]RepoConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Repos []RepoConfig `yaml:"repos"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc.Repos, nil
}

// FilterPR is the exported wrapper for the Stage 2 static filter.
func FilterPR(pr PullRequest, cap int) (keep bool, reason string) { return filterPR(pr, cap) }

func filterPR(pr PullRequest, cap int) (keep bool, reason string) {
	if len(pr.ClosingIssues) == 0 {
		return false, "no-linked-issue"
	}

	hasTestChange := false
	hasCodeChange := false
	hasGeneratedChurn := false
	goModDirs := map[string]bool{}
	for _, f := range pr.Files {
		p := filepath.ToSlash(strings.TrimSpace(f.Path))
		if strings.HasSuffix(p, "_test.go") {
			hasTestChange = true
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			hasCodeChange = true
		}
		if generatedPath(p) {
			hasGeneratedChurn = true
		}
		if filepath.Base(p) == "go.mod" {
			goModDirs[moduleDirForGoMod(p)] = true
		}
	}
	if !hasTestChange {
		return false, "no-test-change"
	}
	if !hasCodeChange {
		return false, "no-code-change"
	}
	if cap > 0 && pr.Additions+pr.Deletions >= cap {
		return false, "diff-too-large"
	}
	if hasGeneratedChurn {
		return false, "generated-churn"
	}
	if len(goModDirs) > 1 {
		return false, "multi-module"
	}
	return true, ""
}

func moduleDirForGoMod(path string) string {
	d := filepath.ToSlash(filepath.Dir(path))
	if d == "." {
		return "."
	}
	return d
}

func generatedPath(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(p)
	return strings.HasPrefix(p, "vendor/") || strings.Contains(p, "/vendor/") ||
		strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, "_generated.go") ||
		strings.HasPrefix(base, "zz_generated") || strings.Contains(p, "code_generated") ||
		strings.Contains(p, "code-generated")
}

// DeriveFailToPass is the exported wrapper for deriveFailToPass.
func DeriveFailToPass(diff string, fileToPkg map[string]string) []OracleTest {
	return deriveFailToPass(diff, fileToPkg)
}

func deriveFailToPass(diff string, fileToPkg map[string]string) []OracleTest {
	type found struct {
		pkg  string
		name string
	}
	var out []found
	seen := map[string]bool{}

	currentFile := ""
	currentTopTest := ""
	suiteToTop := map[string]string{}
	pendingMethods := map[string][]found{}

	flushPending := func(suite string) {
		top := suiteToTop[suite]
		if top == "" {
			return
		}
		for _, m := range pendingMethods[suite] {
			name := top + "/" + m.name
			key := m.pkg + "\x00" + name
			if !seen[key] {
				seen[key] = true
				out = append(out, found{pkg: m.pkg, name: name})
			}
		}
		delete(pendingMethods, suite)
	}

	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ ") {
			currentFile = diffPath(line[4:])
			currentTopTest = ""
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			currentTopTest = ""
			continue
		}
		if currentFile == "" || currentFile == "/dev/null" {
			continue
		}
		pkg := fileToPkg[currentFile]
		if pkg == "" {
			pkg = packageForFile(currentFile)
		}

		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			continue
		}
		content := line
		added := false
		if strings.HasPrefix(content, "+") && !strings.HasPrefix(content, "+++") {
			added = true
			content = content[1:]
		} else if strings.HasPrefix(content, " ") {
			content = content[1:]
		}

		if top := topLevelTestName(content); top != "" {
			currentTopTest = top
			if added {
				key := pkg + "\x00" + top
				if !seen[key] {
					seen[key] = true
					out = append(out, found{pkg: pkg, name: top})
				}
			}
		}
		if suite := suiteRunType(content); suite != "" && currentTopTest != "" {
			suiteToTop[suite] = currentTopTest
			flushPending(suite)
		}
		if !added {
			continue
		}
		if suite, method := suiteMethod(content); suite != "" && method != "" {
			if top := suiteToTop[suite]; top != "" {
				name := top + "/" + method
				key := pkg + "\x00" + name
				if !seen[key] {
					seen[key] = true
					out = append(out, found{pkg: pkg, name: name})
				}
			} else {
				pendingMethods[suite] = append(pendingMethods[suite], found{pkg: pkg, name: method})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].pkg != out[j].pkg {
			return out[i].pkg < out[j].pkg
		}
		return out[i].name < out[j].name
	})
	res := make([]OracleTest, 0, len(out))
	for _, f := range out {
		res = append(res, OracleTest{TestID: TestID{Package: f.pkg, Name: f.name}, RunRegex: "^" + regexp.QuoteMeta(f.name) + "$"})
	}
	return res
}

func diffPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return s
	}
	fields := strings.Fields(s)
	if len(fields) > 0 {
		s = fields[0]
	}
	s = strings.TrimPrefix(s, "a/")
	s = strings.TrimPrefix(s, "b/")
	return filepath.ToSlash(s)
}

var (
	topTestRE     = regexp.MustCompile(`^\s*func\s+(Test\w+)\s*\(`)
	suiteMethodRE = regexp.MustCompile(`^\s*func\s*\(\s*\w+\s+\*?([A-Za-z_]\w*)\s*\)\s*(Test\w+)\s*\(`)
	suiteRunRE    = regexp.MustCompile(`suite\.Run\s*\([^,]+,\s*(?:&|new\()?([A-Za-z_]\w*)`)
)

func topLevelTestName(s string) string {
	m := topTestRE.FindStringSubmatch(s)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func suiteMethod(s string) (string, string) {
	m := suiteMethodRE.FindStringSubmatch(s)
	if len(m) == 3 {
		return m[1], m[2]
	}
	return "", ""
}

func suiteRunType(s string) string {
	m := suiteRunRE.FindStringSubmatch(s)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func packageForFile(path string) string {
	d := filepath.ToSlash(filepath.Dir(path))
	if d == "." {
		return "."
	}
	return "./" + d
}

// ParseGoMod is the exported wrapper for parseGoMod.
func ParseGoMod(s string) (modulePath, goVersion string) { return parseGoMod(s) }

func parseGoMod(s string) (modulePath, goVersion string) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "module":
			modulePath = strings.Trim(fields[1], "\"")
		case "go":
			if goVersion == "" {
				goVersion = fields[1]
			}
		case "toolchain":
			if strings.HasPrefix(fields[1], "go") {
				goVersion = strings.TrimPrefix(fields[1], "go")
			}
		}
	}
	return modulePath, goVersion
}

// TouchedPackages is the exported wrapper for touchedPackages.
func TouchedPackages(files []PRFile) []string { return touchedPackages(files) }

func touchedPackages(files []PRFile) []string {
	set := map[string]bool{}
	for _, f := range files {
		p := filepath.ToSlash(strings.TrimSpace(f.Path))
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		d := filepath.ToSlash(filepath.Dir(p))
		if d == "." {
			set["."] = true
		} else {
			set["./"+d] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// AssembleCandidate is the exported wrapper for assembleCandidate.
func AssembleCandidate(repo RepoConfig, pr PullRequest, repoID int64, baseCommit, goldCommit, mergeMethod, modulePath, goVersion, moduleDir, issueURL, problemStatement string, oracleFiles []string, failToPass []OracleTest, passToPassPackages []string) Instance {
	return assembleCandidate(repo, pr, repoID, baseCommit, goldCommit, mergeMethod, modulePath, goVersion, moduleDir, issueURL, problemStatement, oracleFiles, failToPass, passToPassPackages)
}

func assembleCandidate(repo RepoConfig, pr PullRequest, repoID int64, baseCommit, goldCommit, mergeMethod, modulePath, goVersion, moduleDir, issueURL, problemStatement string, oracleFiles []string, failToPass []OracleTest, passToPassPackages []string) Instance {
	parts := strings.Split(repo.Repo, "/")
	owner, name := "", repo.Repo
	if len(parts) == 2 {
		owner, name = parts[0], parts[1]
	}
	if moduleDir == "" {
		moduleDir = "."
	}
	oracles := append([]string(nil), oracleFiles...)
	sort.Strings(oracles)
	p2p := append([]string(nil), passToPassPackages...)
	sort.Strings(p2p)
	return Instance{
		SchemaVersion:    "gobench.instance.v1",
		InstanceID:       fmt.Sprintf("%s__%s-%d", owner, name, pr.Number),
		Repo:             repo.Repo,
		RepoID:           repoID,
		PRNumber:         pr.Number,
		IssueURL:         issueURL,
		BaseCommit:       baseCommit,
		GoldCommit:       goldCommit,
		MergeMethod:      mergeMethod,
		GoVersion:        goVersion,
		ModulePath:       modulePath,
		ModuleDir:        moduleDir,
		ProblemStatement: problemStatement,
		HintsText:        "",
		OracleFiles:      oracles,
		FailToPass:       failToPass,
		PassToPass:       PassToPass{Packages: p2p},
		Exec:             ExecSpec{Cwd: moduleDir, Argv: []string{"go", "test"}},
		TestTimeout:      "10m",
		Validation:       Validation{RedAtBaseRuns: []RunResult{}, GoldGreenRuns: []RunResult{}, FlakeRuns: 0},
		MinedBy:          "gobench-mine/0.1.0",
		ValidatedAt:      "",
		LicenseNote:      repo.License,
	}
}
