package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

func deriveVerifyCmd(ctx context.Context, sb sandbox.Sandbox, root string) (cmd, provenance string) {
	if sb == nil || root == "" {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, envObserveTimeout)
	defer cancel()

	goRoot := markerExists(ctx, sb, "go.mod")
	rustRoot := markerExists(ctx, sb, "Cargo.toml")
	nodeRoot := markerExists(ctx, sb, "package.json")
	pythonRoot := hasPythonMarker(ctx, sb)
	rootMarkers := 0
	for _, ok := range []bool{goRoot, rustRoot, nodeRoot, pythonRoot} {
		if ok {
			rootMarkers++
		}
	}
	if rootMarkers > 1 {
		return "", ""
	}
	if goRoot {
		return detectGo(ctx, sb)
	}
	if rustRoot {
		return detectRust(ctx, sb)
	}
	if nodeRoot {
		return detectNode(ctx, sb)
	}
	if pythonRoot {
		return detectPython(ctx, sb)
	}
	return detectMake(ctx, sb)
}

func detectGo(ctx context.Context, sb sandbox.Sandbox) (string, string) {
	if !hasCommand(ctx, sb, "go") {
		return "", ""
	}
	return "go build ./... && go test ./...", "go.mod"
}

func detectRust(ctx context.Context, sb sandbox.Sandbox) (string, string) {
	if !hasCommand(ctx, sb, "cargo") {
		return "", ""
	}
	return "cargo build && cargo test", "Cargo.toml"
}

func detectNode(ctx context.Context, sb sandbox.Sandbox) (string, string) {
	pkgJSON, err := sb.ReadFile(ctx, "package.json")
	if err != nil {
		return "", ""
	}
	build, test := nodeTestClause(pkgJSON)
	pm := pkgManager(ctx, sb)
	if !hasCommand(ctx, sb, pm) {
		return "", ""
	}
	if test {
		if build {
			return pm + " run build && " + pm + " run test", "package.json test script"
		}
		return pm + " run test", "package.json test script"
	}
	if markerExists(ctx, sb, "tsconfig.json") && packageHasDep(pkgJSON, "typescript") && hasCommand(ctx, sb, "tsc") {
		return "tsc --noEmit", "tsconfig.json"
	}
	return "", ""
}

func detectPython(ctx context.Context, sb sandbox.Sandbox) (string, string) {
	if pythonImportsPytest(ctx, sb) {
		return "python -m pytest -q", "pytest config"
	}
	if markerExists(ctx, sb, "tests") || markerExists(ctx, sb, "pytest.ini") || markerExists(ctx, sb, "tox.ini") {
		return "", ""
	}
	if hasCommand(ctx, sb, "python") {
		return "python -m compileall .", "python project marker"
	}
	return "", ""
}

func detectMake(ctx context.Context, sb sandbox.Sandbox) (string, string) {
	data, err := sb.ReadFile(ctx, "Makefile")
	if err != nil {
		return "", ""
	}
	if !hasCommand(ctx, sb, "make") {
		return "", ""
	}
	if makeHasTarget(data, "test") {
		return "make test", "Makefile test target"
	}
	if makeHasTarget(data, "check") {
		return "make check", "Makefile check target"
	}
	return "", ""
}

func markerExists(ctx context.Context, sb sandbox.Sandbox, name string) bool {
	if _, err := sb.ReadFile(ctx, name); err == nil {
		return true
	}
	entries, err := sb.ListDir(ctx, ".")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

func hasPythonMarker(ctx context.Context, sb sandbox.Sandbox) bool {
	for _, name := range []string{"pyproject.toml", "setup.cfg", "pytest.ini", "tox.ini"} {
		if markerExists(ctx, sb, name) {
			return true
		}
	}
	return markerExists(ctx, sb, "tests")
}

func nodeTestClause(pkgJSON []byte) (build, test bool) {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(pkgJSON, &pkg); err != nil {
		return false, false
	}
	build = strings.TrimSpace(pkg.Scripts["build"]) != ""
	testScript := strings.TrimSpace(pkg.Scripts["test"])
	if testScript == "" || testScript == `echo "Error: no test specified" && exit 1` {
		return build, false
	}
	return build, true
}

func pkgManager(ctx context.Context, sb sandbox.Sandbox) string {
	switch {
	case markerExists(ctx, sb, "pnpm-lock.yaml"):
		return "pnpm"
	case markerExists(ctx, sb, "yarn.lock"):
		return "yarn"
	case markerExists(ctx, sb, "bun.lockb"):
		return "bun"
	default:
		return "npm"
	}
}

var hasCommand = func(ctx context.Context, sb sandbox.Sandbox, tool string) bool {
	out, err := runOp(ctx, sb, "command -v "+tool, envObserveTimeout)
	return err == nil && !isRunFailure(out)
}

func pythonImportsPytest(ctx context.Context, sb sandbox.Sandbox) bool {
	out, err := runOp(ctx, sb, "python -c 'import pytest'", envObserveTimeout)
	return err == nil && !isRunFailure(out)
}

func packageHasDep(pkgJSON []byte, dep string) bool {
	var pkg struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(pkgJSON, &pkg); err != nil {
		return false
	}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.PeerDependencies, pkg.OptionalDependencies} {
		if _, ok := deps[dep]; ok {
			return true
		}
	}
	return false
}

func makeHasTarget(data []byte, target string) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `\s*:`)
	return re.Match(data)
}
