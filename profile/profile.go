package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

var cleanEnvAllowlist = []string{
	"PATH", "HOME", "USER", "SHELL", "LANG", "LC_ALL", "TERM", "TMPDIR",
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN", "GOPROXY",
}

// CleanEnvAllowlist returns a fresh copy of the conservative clean-environment
// baseline for reviewed-local. It contains names only, not values; provider keys
// and tokens are deliberately absent. Content changes are policy changes
// (PROFILES.md §1.2).
func CleanEnvAllowlist() []string {
	return append([]string(nil), cleanEnvAllowlist...)
}

// Descriptor owns extraction, satisfaction, and violation rendering for one
// typed trust-floor dimension from docs/specs/PROFILES.md §3.
type Descriptor struct {
	name      string
	satisfies func(candidate, floor Posture) bool
	violation func(candidate, floor Posture) string
}

// Name is the stable trust-tag name of this floor dimension.
func (d Descriptor) Name() string { return d.name }

var descriptors = []Descriptor{
	{
		name:      "sandbox",
		satisfies: func(c, f Posture) bool { return c.Sandbox.Satisfies(f.Sandbox) },
		violation: func(c, f Posture) string {
			return fmt.Sprintf("sandbox candidate %s does not meet floor %s", c.Sandbox, f.Sandbox)
		},
	},
	{
		name:      "min-isolation",
		satisfies: func(c, f Posture) bool { return c.MinIsolation >= f.MinIsolation },
		violation: func(c, f Posture) string {
			return fmt.Sprintf("min-isolation candidate %s does not meet floor %s", c.MinIsolation, f.MinIsolation)
		},
	},
	{
		name:      "worktree",
		satisfies: func(c, f Posture) bool { return c.Worktree.Satisfies(f.Worktree) },
		violation: func(c, f Posture) string {
			return fmt.Sprintf("worktree candidate %s does not meet floor %s", c.Worktree, f.Worktree)
		},
	},
	{
		name:      "approval",
		satisfies: func(c, f Posture) bool { return c.Approval.Satisfies(f.Approval) },
		violation: func(c, f Posture) string {
			return fmt.Sprintf("approval candidate %s does not meet floor %s", c.Approval, f.Approval)
		},
	},
	{
		name:      "secrets",
		satisfies: func(c, f Posture) bool { return c.Secrets.Satisfies(f.Secrets) },
		violation: func(c, f Posture) string {
			return fmt.Sprintf("secrets candidate %s does not meet floor %s", c.Secrets, f.Secrets)
		},
	},
	{
		name:      "network",
		satisfies: func(c, f Posture) bool { return c.Network.Satisfies(f.Network) },
		violation: func(c, f Posture) string {
			return fmt.Sprintf("network candidate %s does not meet floor %s", c.Network, f.Network)
		},
	},
}

// Descriptors returns the six ordered floor descriptors. A copy prevents a
// caller from changing the resolver's table.
func Descriptors() []Descriptor { return append([]Descriptor(nil), descriptors...) }

// Enforce rejects a candidate that weakens any floor dimension of t.
func Enforce(t Trust, candidate Posture) error {
	if _, err := ParseTrust(string(t)); err != nil {
		return err
	}
	floor := FloorFor(t)
	for _, d := range descriptors {
		if !d.satisfies(candidate, floor) {
			return fmt.Errorf("trust %q violates %s: %s", t, d.Name(), d.violation(candidate, floor))
		}
	}
	return nil
}

// ApprovalPolicy is a versioned, closed-registry default-deny command policy.
// Allowlist records the command shapes that are part of its canonical identity;
// authorization itself is performed over a parsed shell AST.
type ApprovalPolicy struct {
	Name      string
	Version   string
	Allowlist []string
}

// DefaultHeadlessPolicy is the shipped conservative policy for headless
// reviewed-local execution. Older versions remain frozen in the registry for
// persisted identity lookup, but are never selected as the default.
const DefaultHeadlessPolicy = "reviewed-local-readonly-v3"

var approvalPolicies = map[string]ApprovalPolicy{
	"reviewed-local-readonly-v1": {
		Name: "reviewed-local-readonly-v1", Version: "1",
		Allowlist: []string{"go test", "go build", "go vet", "git status", "git diff", "ls", "cat"},
	},
	"reviewed-local-readonly-v2": {
		Name: "reviewed-local-readonly-v2", Version: "2",
		Allowlist: []string{"go test [safe args]", "go build [safe args]", "go vet [safe args]", "git status [safe args]", "git diff [safe args]"},
	},
	DefaultHeadlessPolicy: {
		Name: DefaultHeadlessPolicy, Version: "3",
		Allowlist: []string{"go test [safe args]", "go build [safe args]", "go vet [safe args]", "git status [safe args]", "git diff [safe args]", "rg [search-tool args]"},
	},
}

// PolicyByName returns a copy of a shipped policy. The registry is closed so
// policy identity cannot be weakened by arbitrary operator-supplied content.
func PolicyByName(name string) (ApprovalPolicy, error) {
	p, ok := approvalPolicies[name]
	if !ok {
		return ApprovalPolicy{}, fmt.Errorf("unknown approval policy %q", name)
	}
	p.Allowlist = append([]string(nil), p.Allowlist...)
	return p, nil
}

// Hash returns the SHA-256 hex digest of the policy's canonical JSON content.
func (p ApprovalPolicy) Hash() string {
	// A struct, rather than a map, fixes key ordering. The allowlist order is
	// policy content and is therefore intentionally preserved in the hash.
	content := struct {
		Name      string   `json:"name"`
		Version   string   `json:"version"`
		Allowlist []string `json:"allowlist"`
	}{p.Name, p.Version, p.Allowlist}
	encoded, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

var safeGoFlag = regexp.MustCompile(`^-(?:race|short|v|json|count=[0-9]+|run=[A-Za-z0-9_./^$|*+?()\[\]{}-]+)$`)

// Allows authorizes exactly one parsed simple shell command. It deliberately
// does not authorize by textual prefix: every AST node and argument is checked,
// while sh -c remains only the run tool's transport.
func (p ApprovalPolicy) Allows(src string) bool {
	if !((p.Name == "reviewed-local-readonly-v2" && p.Version == "2") ||
		(p.Name == DefaultHeadlessPolicy && p.Version == "3")) {
		return false // v1 is retained as identity data, not executable policy.
	}
	argv, ok := parseSimpleCommand(src)
	if !ok || len(argv) < 2 {
		return false
	}
	switch argv[0] {
	case "go":
		if argv[1] != "test" && argv[1] != "build" && argv[1] != "vet" {
			return false
		}
		for _, arg := range argv[2:] {
			if strings.HasPrefix(arg, "-") {
				if !safeGoFlag.MatchString(arg) {
					return false
				}
			} else if !workspacePath(arg) {
				return false
			}
		}
		return true
	case "git":
		switch argv[1] {
		case "status":
			for _, arg := range argv[2:] {
				if arg != "--short" && arg != "--porcelain" && arg != "--branch" {
					return false
				}
			}
			return true
		case "diff":
			paths := false
			for _, arg := range argv[2:] {
				if arg == "--" {
					paths = true
				} else if paths {
					if !workspacePath(arg) {
						return false
					}
				} else if arg != "--stat" && arg != "--name-only" && arg != "--name-status" && arg != "--cached" && arg != "--staged" {
					return false
				}
			}
			return true
		}
	}
	return false
}

// AllowsExec authorizes typed, non-shell execution used by harness tools. Older
// policy versions intentionally have no direct-exec channel.
func (p ApprovalPolicy) AllowsExec(path string, args []string) bool {
	if p.Name != DefaultHeadlessPolicy || p.Version != "3" || path != "rg" {
		return false
	}
	want := []string{"--line-number", "--no-heading", "--color=never", "--smart-case", "--max-columns=250", "--max-columns-preview", "-e"}
	if len(args) < len(want)+1 {
		return false // the pattern must immediately follow -e.
	}
	for i := range want {
		if args[i] != want[i] {
			return false
		}
	}
	remaining := args[len(want)+1:]
	if len(remaining) == 0 {
		return true
	}
	if remaining[0] != "--" || len(remaining) == 1 {
		return false
	}
	for _, path := range remaining[1:] {
		if strings.HasPrefix(path, "-") || !workspacePath(path) {
			return false
		}
	}
	return true
}

func parseSimpleCommand(src string) ([]string, bool) {
	f, err := syntax.NewParser(syntax.KeepComments(true)).Parse(strings.NewReader(src), "approval")
	if err != nil || len(f.Stmts) != 1 || len(f.Last) != 0 {
		return nil, false
	}
	s := f.Stmts[0]
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || s.Semicolon.IsValid() || s.Negated || s.Background || s.Coprocess || len(s.Redirs) != 0 || len(s.Comments) != 0 || len(call.Assigns) != 0 {
		return nil, false
	}
	argv := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		value, ok := literalWord(word)
		if !ok {
			return nil, false
		}
		argv = append(argv, value)
	}
	return argv, len(argv) != 0
}

func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	var parts func([]syntax.WordPart, bool) bool
	parts = func(ps []syntax.WordPart, quoted bool) bool {
		for _, part := range ps {
			switch x := part.(type) {
			case *syntax.Lit:
				// Unquoted literals can trigger pathname, tilde, or brace expansion.
				if !quoted && strings.ContainsAny(x.Value, "*?[~{}") {
					return false
				}
				b.WriteString(x.Value)
			case *syntax.SglQuoted:
				if x.Dollar {
					return false
				}
				b.WriteString(x.Value)
			case *syntax.DblQuoted:
				if x.Dollar || !parts(x.Parts, true) {
					return false
				}
			default:
				return false // expansions, substitutions, process substitution, extglob, etc.
			}
		}
		return true
	}
	if !parts(w.Parts, false) {
		return "", false
	}
	return b.String(), true
}

func workspacePath(arg string) bool {
	if arg == "" || filepath.IsAbs(arg) {
		return false
	}
	clean := filepath.Clean(arg)
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
