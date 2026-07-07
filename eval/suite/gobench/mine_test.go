package gobench

import (
	"reflect"
	"testing"
)

func TestFilterPR(t *testing.T) {
	base := PullRequest{
		Number:        1,
		Additions:     10,
		Deletions:     5,
		ClosingIssues: []int{123},
		Files: []PRFile{
			{Path: "pkg/foo.go"},
			{Path: "pkg/foo_test.go"},
		},
	}

	cases := []struct {
		name   string
		pr     PullRequest
		keep   bool
		reason string
	}{
		{name: "keep", pr: base, keep: true},
		{name: "no linked issue", pr: func() PullRequest { p := base; p.ClosingIssues = nil; return p }(), reason: "no-linked-issue"},
		{name: "no test change", pr: func() PullRequest { p := base; p.Files = []PRFile{{Path: "pkg/foo.go"}}; return p }(), reason: "no-test-change"},
		{name: "no code change", pr: func() PullRequest { p := base; p.Files = []PRFile{{Path: "pkg/foo_test.go"}}; return p }(), reason: "no-code-change"},
		{name: "diff too large", pr: func() PullRequest { p := base; p.Additions = 399; p.Deletions = 1; return p }(), reason: "diff-too-large"},
		{name: "generated churn", pr: func() PullRequest { p := base; p.Files = append(p.Files, PRFile{Path: "pkg/api.pb.go"}); return p }(), reason: "generated-churn"},
		{name: "multi module", pr: func() PullRequest {
			p := base
			p.Files = append(p.Files, PRFile{Path: "go.mod"}, PRFile{Path: "tools/go.mod"})
			return p
		}(), reason: "multi-module"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keep, reason := filterPR(tc.pr, 400)
			if keep != tc.keep || reason != tc.reason {
				t.Fatalf("filterPR = (%v, %q), want (%v, %q)", keep, reason, tc.keep, tc.reason)
			}
		})
	}
}

func TestDeriveFailToPass(t *testing.T) {
	diff := `diff --git a/pkg/foo_test.go b/pkg/foo_test.go
index 111..222 100644
--- a/pkg/foo_test.go
+++ b/pkg/foo_test.go
@@ -1,3 +1,7 @@
 package pkg
+func TestNewBehavior(t *testing.T) {
+}
 func TestStoreSuite(t *testing.T) {
     suite.Run(t, new(StoreSuite))
 }
+func (s *StoreSuite) TestRepairsBug() {
+}
`
	got := deriveFailToPass(diff, map[string]string{"pkg/foo_test.go": "./pkg"})
	want := []OracleTest{
		{TestID: TestID{Package: "./pkg", Name: "TestNewBehavior"}, RunRegex: "^TestNewBehavior$"},
		{TestID: TestID{Package: "./pkg", Name: "TestStoreSuite/TestRepairsBug"}, RunRegex: "^TestStoreSuite/TestRepairsBug$"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deriveFailToPass = %#v, want %#v", got, want)
	}
}

func TestParseGoMod(t *testing.T) {
	modulePath, goVersion := parseGoMod(`module github.com/example/project

go 1.22.0

toolchain go1.22.3
`)
	if modulePath != "github.com/example/project" || goVersion != "1.22.3" {
		t.Fatalf("parseGoMod = (%q, %q)", modulePath, goVersion)
	}
}

func TestTouchedPackages(t *testing.T) {
	got := touchedPackages([]PRFile{
		{Path: "main.go"},
		{Path: "pkg/a.go"},
		{Path: "pkg/a_test.go"},
		{Path: "pkg/sub/b.go"},
	})
	want := []string{".", "./pkg", "./pkg/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("touchedPackages = %#v, want %#v", got, want)
	}
}

func TestLoadReposRealFile(t *testing.T) {
	repos, err := LoadRepos("repos.yaml")
	if err != nil {
		t.Fatalf("LoadRepos: %v", err)
	}
	if len(repos) <= 5 {
		t.Fatalf("LoadRepos returned %d repos, want > 5", len(repos))
	}
	found := false
	for _, r := range repos {
		if r.Repo == "open-policy-agent/opa" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("LoadRepos did not include open-policy-agent/opa")
	}
}

func TestAssembleCandidateValidates(t *testing.T) {
	inst := assembleCandidate(
		RepoConfig{Repo: "owner/name", License: "MIT"},
		PullRequest{Number: 42},
		1234,
		"base-sha",
		"gold-sha",
		"merge",
		"github.com/owner/name",
		"1.22.3",
		".",
		"https://github.com/owner/name/issues/1",
		"raw issue body",
		[]string{"pkg/foo_test.go"},
		[]OracleTest{{TestID: TestID{Package: "./pkg", Name: "TestFix"}, RunRegex: "^TestFix$"}},
		[]string{"./pkg"},
	)
	if err := inst.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if inst.SchemaVersion != "gobench.instance.v1" || inst.Validation.FlakeRuns != 0 || inst.ValidatedAt != "" || inst.GoVersion == "" {
		t.Fatalf("candidate has wrong miner/validator fields: %#v", inst)
	}
}
