package gobench

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// GraderVersion is the version string emitted in every verdict.
const GraderVersion = "gobench-grade/0.1.0"

// Instance is one GoBench task: a single real Go bug, mined from a merged PR,
// validated red-at-base / gold-green, distributed as metadata only.
type Instance struct {
	SchemaVersion string `json:"schema_version"`

	InstanceID string `json:"instance_id"`
	Repo       string `json:"repo"`
	RepoID     int64  `json:"repo_id"`
	PRNumber   int    `json:"pr_number"`
	IssueURL   string `json:"issue_url"`

	BaseCommit  string `json:"base_commit"`
	GoldCommit  string `json:"gold_commit"`
	MergeMethod string `json:"merge_method"`
	GoVersion   string `json:"go_version"`
	ModulePath  string `json:"module_path"`
	ModuleDir   string `json:"module_dir"`

	ProblemStatement string `json:"problem_statement"`
	HintsText        string `json:"hints_text"`

	OracleFiles []string     `json:"oracle_files"`
	FailToPass  []OracleTest `json:"FAIL_TO_PASS"`
	PassToPass  PassToPass   `json:"PASS_TO_PASS"`
	Exec        ExecSpec     `json:"exec"`
	TestTimeout string       `json:"test_timeout"`

	Validation Validation `json:"validation"`

	MinedBy     string   `json:"mined_by"`
	ValidatedAt string   `json:"validated_at"`
	LicenseNote string   `json:"license_note"`
	Aliases     []string `json:"aliases,omitempty"`
}

// TestID is the package-qualified identity of a Go test within the module.
type TestID struct {
	Package string `json:"package"`
	Name    string `json:"name"`
	Subtest string `json:"subtest,omitempty"`
}

// OracleTest is a FAIL_TO_PASS entry: a TestID plus the anchored -run regex.
type OracleTest struct {
	TestID
	RunRegex string `json:"run_regex"`
}

// PassToPass is package-level regression coverage.
type PassToPass struct {
	Packages []string `json:"packages"`
	RunRegex string   `json:"run_regex,omitempty"`
}

// ExecSpec pins hermetic execution so every grader runs tests identically.
type ExecSpec struct {
	Cwd       string   `json:"cwd"`
	Argv      []string `json:"argv"`
	Env       []string `json:"env,omitempty"`
	BuildTags []string `json:"build_tags,omitempty"`
}

// Validation is the determinism receipt.
type Validation struct {
	RedAtBaseRuns []RunResult `json:"red_at_base_runs"`
	GoldGreenRuns []RunResult `json:"gold_green_runs"`
	FlakeRuns     int         `json:"flake_runs"`
	CreatedAt     string      `json:"created_at"`
	LeakScreen    LeakScreen  `json:"leak_screen"`
	Demotions     []Demotion  `json:"demotions,omitempty"`
	ValidatorVer  string      `json:"validator_version"`
}

type Demotion struct {
	Test      TestID `json:"test"`
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason"`
	DemotedAt string `json:"demoted_at"`
}

type RunResult struct {
	Passed    bool     `json:"passed"`
	RanTests  []TestID `json:"ran_tests"`
	DurationS float64  `json:"duration_s"`
}

// LeakScreen records that problem_statement was checked for fix leakage.
type LeakScreen struct {
	Method        string   `json:"method"`
	NgramSize     int      `json:"ngram_size"`
	Tokenization  string   `json:"tokenization"`
	Passed        bool     `json:"passed"`
	Score         float64  `json:"score"`
	Threshold     float64  `json:"threshold"`
	DiffRange     string   `json:"diff_range"`
	ExcludedFiles []string `json:"excluded_files,omitempty"`
	StatementHash string   `json:"statement_hash"`
	GoldDiffHash  string   `json:"gold_diff_hash"`
	ToolVersion   string   `json:"tool_version"`
	ScreenedAt    string   `json:"screened_at"`
}

// Verdict is the machine-readable grader output record.
type Verdict struct {
	InstanceID    string   `json:"instance_id"`
	Resolved      bool     `json:"resolved"`
	F2PPass       bool     `json:"f2p_pass"`
	P2PPass       bool     `json:"p2p_pass"`
	TestbuildOK   bool     `json:"testbuild_ok"`
	RanTests      []TestID `json:"ran_tests"`
	GraderError   string   `json:"grader_error,omitempty"`
	GraderVersion string   `json:"grader_version"`
}

// LoadInstance loads an instance JSON file and rejects schema major versions
// other than v1 (for example, gobench.instance.v1 is accepted).
func LoadInstance(path string) (Instance, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Instance{}, err
	}
	var inst Instance
	if err := json.Unmarshal(b, &inst); err != nil {
		return Instance{}, err
	}
	if schemaMajor(inst.SchemaVersion) != "v1" {
		return Instance{}, fmt.Errorf("unsupported schema_version %q", inst.SchemaVersion)
	}
	return inst, nil
}

// Validate enforces structural v1 constraints. A stricter release-readiness
// check (requiring gold_commit and full validation) is deferred to the Phase 3 validator.
func (i Instance) Validate() error {
	if i.InstanceID == "" {
		return fmt.Errorf("instance_id is empty")
	}
	if schemaMajor(i.SchemaVersion) != "v1" {
		return fmt.Errorf("%s: schema_version must be v1, got %q", i.InstanceID, i.SchemaVersion)
	}
	if i.Repo == "" {
		return fmt.Errorf("%s: repo is empty", i.InstanceID)
	}
	if i.BaseCommit == "" {
		return fmt.Errorf("%s: base_commit is empty", i.InstanceID)
	}
	if len(i.FailToPass) == 0 {
		return fmt.Errorf("%s: FAIL_TO_PASS is empty", i.InstanceID)
	}
	for _, ot := range i.FailToPass {
		if ot.Package == "" {
			return fmt.Errorf("%s: FAIL_TO_PASS entry has empty package", i.InstanceID)
		}
		if ot.Name == "" {
			return fmt.Errorf("%s: FAIL_TO_PASS entry has empty name", i.InstanceID)
		}
		if !strings.HasPrefix(ot.RunRegex, "^") || !strings.HasSuffix(ot.RunRegex, "$") {
			return fmt.Errorf("%s: FAIL_TO_PASS entry run_regex %q must start with ^ and end with $", i.InstanceID, ot.RunRegex)
		}
	}
	for _, p := range i.PassToPass.Packages {
		if p == "" {
			return fmt.Errorf("%s: PASS_TO_PASS entry is empty", i.InstanceID)
		}
	}
	if len(i.Exec.Argv) == 0 {
		return fmt.Errorf("%s: exec.argv is empty", i.InstanceID)
	}
	for _, arg := range i.Exec.Argv {
		if arg == "-run" || strings.HasPrefix(arg, "-run=") {
			return fmt.Errorf("%s: exec.argv contains forbidden -run", i.InstanceID)
		}
		if arg == "-tags" || strings.HasPrefix(arg, "-tags=") {
			return fmt.Errorf("%s: exec.argv contains forbidden -tags", i.InstanceID)
		}
		if strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "/") || strings.Contains(arg, "/") {
			return fmt.Errorf("%s: exec.argv contains forbidden package pattern %q", i.InstanceID, arg)
		}
	}
	if i.Validation.FlakeRuns > 0 {
		if len(i.Validation.RedAtBaseRuns) != i.Validation.FlakeRuns {
			return fmt.Errorf("%s: validation.red_at_base_runs length %d != flake_runs %d", i.InstanceID, len(i.Validation.RedAtBaseRuns), i.Validation.FlakeRuns)
		}
		if len(i.Validation.GoldGreenRuns) != i.Validation.FlakeRuns {
			return fmt.Errorf("%s: validation.gold_green_runs length %d != flake_runs %d", i.InstanceID, len(i.Validation.GoldGreenRuns), i.Validation.FlakeRuns)
		}
	}
	return nil
}

func schemaMajor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
