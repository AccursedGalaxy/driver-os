package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Go knowledge tools. Unlike every other tool, go_doc and the dependency-source
// reads run HOST-SIDE (not through the sandbox): looking up the API or source of a
// pinned dependency is READ-ONLY, TRUSTED introspection — not untrusted code
// execution — so it has no business behind the isolation boundary, and running it
// host-side is the only thing that works on EVERY backend (a network-off container
// has neither the toolchain nor the module cache). This mirrors how `search`
// always scopes to the root: a deliberately-trusted capability layered above the
// fence, never a hole punched through it. The fence stays exactly as strict for
// the workspace; these reads can only ever READ the toolchain + module cache.

// goDocTimeout caps a single `go doc`/`go list` invocation. Doc lookups are fast;
// this only guards against a pathological package load hanging the turn.
const goDocTimeout = 30 * time.Second

// goDocAllowedFlags whitelists the `go doc` flags the tool passes through — all
// read-only presentation toggles. -all: the whole package API. -src: the symbol's
// actual source. -u: include unexported. -c: case-sensitive symbol match. Anything
// else is rejected so the arg can't smuggle a flag into the host `go` process.
var goDocAllowedFlags = map[string]bool{"-all": true, "-src": true, "-u": true, "-c": true}

// hostWorkspace returns the workspace directory on the HOST filesystem — where the
// Go toolchain and module cache live — so host-side Go introspection runs with the
// right cwd to resolve module versions against the project's go.mod. The local
// sandbox exposes Root() (the host workspace); other backends fall back to the
// process cwd, which IS the workspace for the single-agent entrypoints (cmd/agent
// runs there). A future docker/gated Root() passthrough would make multi-sandbox
// hosts exact, but is not needed for the local default.
func hostWorkspace(sb sandbox.Sandbox) string {
	if r, ok := sb.(interface{ Root() string }); ok {
		if root := r.Root(); root != "" {
			return root
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// parseGoDocArg splits a `go doc`-style argument line into (flags, target), where
// target is [pkg] or [pkg, symbol]. Only whitelisted flags are accepted and they
// must precede the package; an unknown flag, a missing package, or too many words
// is a usage error whose message teaches the form (P3).
func parseGoDocArg(arg string) (flags, target []string, err error) {
	for _, tok := range strings.Fields(arg) {
		if strings.HasPrefix(tok, "-") {
			if len(target) > 0 {
				return nil, nil, fmt.Errorf("flags must come before the package: unexpected %q after the package name", tok)
			}
			if !goDocAllowedFlags[tok] {
				return nil, nil, fmt.Errorf("unsupported flag %q — allowed: -all, -src, -u, -c", tok)
			}
			flags = append(flags, tok)
			continue
		}
		target = append(target, tok)
	}
	if len(target) == 0 {
		return nil, nil, fmt.Errorf("name a package (and optional symbol), e.g. `strings.Builder`, `-src io.Copy`, or `github.com/openai/openai-go ChatCompletionNewParams`")
	}
	if len(target) > 2 {
		return nil, nil, fmt.Errorf("too many arguments — use `<package> [symbol]`, e.g. `net/http Client`")
	}
	return flags, target, nil
}

// goDocRun runs `go doc` host-side and appends a `source: <dir>` line pointing at
// the package's on-disk source (so the agent can read_file/search the full source
// next). It resolves against the workspace go.mod, so stdlib and every pinned
// dependency render at the EXACT version the compiler sees. A non-zero exit with
// no output is a real error; partial output (go doc warns but still prints) is
// returned as the observation.
func goDocRun(ctx context.Context, workdir string, flags, target []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, goDocTimeout)
	defer cancel()

	args := append([]string{"doc"}, flags...)
	args = append(args, target...)
	out, err := runGoTool(ctx, workdir, args)
	if err != nil && strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("go doc %s failed: %v", strings.Join(append(flags, target...), " "), strings.TrimSpace(err.Error()))
	}
	res := strings.TrimRight(out, "\n")
	if dir := goPkgDir(ctx, workdir, docPackage(target)); dir != "" {
		res += fmt.Sprintf("\n\nsource: %s\n(read whole files there with read_file / search — that path is read-only)", dir)
	}
	return truncate(res), nil
}

// goDocArg is the text-protocol entry: parse the arg, then run.
func goDocArg(ctx context.Context, workdir, arg string) (string, error) {
	flags, target, err := parseGoDocArg(arg)
	if err != nil {
		return "", err
	}
	return goDocRun(ctx, workdir, flags, target)
}

// runGoTool runs a `go` subcommand host-side with the network disabled
// (GOPROXY=off) and the module graph left untouched (-mod=readonly — a lookup must
// never rewrite the workspace go.mod/go.sum). cwd=workdir resolves versions
// against the project. Returns combined stdout+stderr so a model sees go's own
// diagnostics on failure.
func runGoTool(ctx context.Context, workdir string, args []string) (string, error) {
	c := exec.CommandContext(ctx, "go", args...)
	c.Dir = workdir
	c.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly")
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.String(), err
}

// docPackage extracts the PACKAGE import path from a go-doc target, dropping any
// fused symbol — `go list` needs a package, not a `pkg.Symbol`. It mirrors go
// doc's own grammar: with a separate symbol token the package is target[0]; in the
// single-token form, a dot in the FINAL path segment starts the symbol (an import
// path's last segment is a Go identifier, so it never contains a dot itself).
// "strings.Builder"→"strings", "encoding/json.Marshal"→"encoding/json",
// "github.com/openai/openai-go"→unchanged.
func docPackage(target []string) string {
	tok := target[0]
	if len(target) == 2 {
		return tok
	}
	slash := strings.LastIndex(tok, "/")
	base := tok[slash+1:]
	if dot := strings.Index(base, "."); dot >= 0 {
		return tok[:slash+1] + base[:dot]
	}
	return tok
}

// goPkgDir resolves a package's on-disk directory (under GOROOT or the module
// cache) via `go list`, best-effort — empty string if go can't load it standalone.
func goPkgDir(ctx context.Context, workdir, pkg string) string {
	out, err := runGoTool(ctx, workdir, []string{"list", "-f", "{{.Dir}}", pkg})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// --- read-only dependency / stdlib source browsing ------------------------
//
// The read tools (read_file, list_dir, search) accept an ABSOLUTE path under the
// module cache (GOMODCACHE) or the Go toolchain source (GOROOT) — the paths
// go_doc surfaces on its `source:` line — and serve it HOST-SIDE, read-only. This
// is NOT a hole in the fence: writes still go through the sandbox and refuse any
// absolute path; only reads of the (read-only) Go source trees are allowed, and
// only host-side, so it works identically on every backend. A relative path is
// never a go-source path — it resolves inside the workspace fence as before.

var (
	goRootsOnce sync.Once
	goRoots     []string // cleaned [GOMODCACHE, GOROOT]; the read-only Go source trees.
)

// goSourceRoots returns the canonical module-cache and GOROOT directories, asked
// of the toolchain once and cached (they're host-global and don't change within a
// process). On any failure the slice is empty, which simply means no path is ever
// treated as go-source and everything stays fenced — fail safe.
func goSourceRoots() []string {
	goRootsOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMODCACHE", "GOROOT").Output()
		if err != nil {
			return
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if d := strings.TrimSpace(line); d != "" {
				goRoots = append(goRoots, filepath.Clean(d))
			}
		}
	})
	return goRoots
}

// goSourcePath reports whether an ABSOLUTE path points into a read-only Go source
// tree (GOMODCACHE or GOROOT), returning the cleaned path. It cleans first, so a
// `..` cannot traverse out of the tree before the prefix check (e.g.
// "<modcache>/../../etc/passwd" cleans above the root and is rejected). Relative
// paths return false — they belong to the workspace fence.
func goSourcePath(path string) (string, bool) {
	p := strings.TrimSpace(path)
	if !filepath.IsAbs(p) {
		return "", false
	}
	clean := filepath.Clean(p)
	for _, root := range goSourceRoots() {
		if root == "" {
			continue
		}
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return clean, true
		}
	}
	return "", false
}

// readHostFileBounded reads at most maxFileBytes off the host (P4 — no OOM on a
// giant generated file), reporting whether it hit the cap. It is the host-side
// twin of readBounded's sandbox path, used only for paths goSourcePath has already
// confined to the read-only Go trees.
func readHostFileBounded(path string) (data []byte, overCap bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > maxFileBytes {
		return data[:maxFileBytes], true, nil
	}
	return data, false, nil
}

// hostListDir lists a host directory in the read-only Go trees, returning the same
// DirEntry shape the sandbox produces so listDirOp formats it identically.
func hostListDir(dir string) ([]sandbox.DirEntry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.DirEntry, 0, len(des))
	for _, de := range des {
		out = append(out, sandbox.DirEntry{Name: de.Name(), IsDir: de.IsDir()})
	}
	return out, nil
}

// hostSearch runs ripgrep host-side against a directory in the read-only Go trees,
// then reuses formatSearch so a dependency search reads exactly like a project
// search (same caps, same `path:line:text`, same no-match phrasing).
func hostSearch(ctx context.Context, pattern, dir string, timeout time.Duration) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", fmt.Errorf("search needs a pattern, e.g. `search retry|backoff`")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	args := []string{
		"--line-number", "--no-heading", "--color=never", "--smart-case",
		"--max-columns=250", "--max-columns-preview", "-e", pattern, "--", dir,
	}
	c := exec.CommandContext(ctx, "rg", args...)
	var so, se bytes.Buffer
	c.Stdout, c.Stderr = &so, &se
	err := c.Run()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("ripgrep (rg) is not installed — fall back to `run grep -rn <pattern> %s`", dir)
		}
		// rg exits 1 on "no matches" — that's a *Result, not a Go error; only a
		// failure to PRODUCE an exit status (e.g. ctx canceled) lacks ProcessState.
		if c.ProcessState == nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("search timed out after %s — narrow it with a more specific pattern", timeout)
			}
			return "", err
		}
	}
	res := &sandbox.Result{ExitCode: c.ProcessState.ExitCode(), Stdout: so.Bytes(), Stderr: se.Bytes()}
	return formatSearch(res, pattern, dir), nil
}
