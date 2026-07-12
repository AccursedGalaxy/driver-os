package profile

import "testing"

func TestApprovalPolicyV2CommandShapes(t *testing.T) {
	p, err := PolicyByName(DefaultHeadlessPolicy)
	if err != nil {
		t.Fatal(err)
	}

	allowed := []string{
		"go test", "go test ./...", "go test -race -count=1 ./internal/profile",
		"go build ./...", "go vet -json ./...",
		"git status", "git status --short --branch",
		"git diff", "git diff --stat --cached", "git diff -- src/file.go",
		`go test -run='TestOne|TestTwo' ./...`,
	}
	for _, command := range allowed {
		t.Run("allow/"+command, func(t *testing.T) {
			if !p.Allows(command) {
				t.Errorf("Allows(%q) = false", command)
			}
		})
	}

	denied := []string{
		`cat "$(touch /tmp/pwned)"`,
		"cat `touch /tmp/pwned`",
		"cat file > /tmp/pwned",
		"go test $(malicious)",
		"go test ./... | malicious",
		"go test ./... && malicious",
		"go test ./... || malicious",
		"go test ./...; malicious",
		"go test ./...\nmalicious",
		"go test ./... # hidden syntax",
		"go test /tmp/outside", "go test ../../outside",
		"go test ./... &", "X=go go test ./...", "go $CMD ./...",
		"go test ./.../*", "go test -exec=/tmp/malicious ./...",
		"go build -o=/tmp/pwned ./...", "git diff --no-index /etc/passwd /etc/shadow",
		"cat file", "ls", "rm -rf /",
	}
	for _, command := range denied {
		t.Run("deny/"+command, func(t *testing.T) {
			if p.Allows(command) {
				t.Errorf("Allows(%q) = true", command)
			}
		})
	}
}

func TestApprovalPolicyV3ExecShapes(t *testing.T) {
	p, err := PolicyByName(DefaultHeadlessPolicy)
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"--line-number", "--no-heading", "--color=never", "--smart-case", "--max-columns=250", "--max-columns-preview", "-e", "needle"}
	if !p.AllowsExec("rg", base) || !p.AllowsExec("rg", append(append([]string{}, base...), "--", "agent")) {
		t.Fatal("v3 denied the search tool's exact rg shape")
	}
	denied := [][]string{
		{"--pre", "cat", "-e", "needle"},
		append(append([]string{}, base...), "agent"),
		append(append([]string{}, base...), "--", "--pre"),
	}
	for _, args := range denied {
		if p.AllowsExec("rg", args) {
			t.Errorf("AllowsExec(rg, %q) = true", args)
		}
	}
	if p.AllowsExec("rm", []string{"-f", "x"}) || p.AllowsExec("mkdir", []string{"-p", "x"}) {
		t.Fatal("v3 authorized a harness housekeeping exec")
	}

	v2, err := PolicyByName("reviewed-local-readonly-v2")
	if err != nil {
		t.Fatal(err)
	}
	if v2.AllowsExec("rg", base) {
		t.Fatal("v2 must not authorize direct execution")
	}
}

func TestApprovalPolicyV1IdentityRetainedButDisabled(t *testing.T) {
	p, err := PolicyByName("reviewed-local-readonly-v1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "1" || p.Hash() == "" {
		t.Fatalf("bad retained identity: %+v", p)
	}
	if p.Allows("go test ./...") {
		t.Fatal("retired vulnerable policy must not authorize execution")
	}
}
