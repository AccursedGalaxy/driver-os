// Package local is the IsolationNone sandbox backend: it runs commands as child
// processes on the host and confines file access to a root directory with a path
// fence. It is the lifted, generalized form of cmd/agent's old confineToRoot.
//
// IsolationNone means there is NO real isolation for Exec — a shell command runs
// on the host with the agent's own privileges; the fence only governs the
// ReadFile/WriteFile/ListDir paths, not what a spawned process can touch. So this
// backend is for TRUSTED code only (code we wrote). Running untrusted,
// model-authored code requires a stronger backend (container/gVisor/microVM)
// behind the same sandbox.Sandbox interface — see ../../docs/specs/SANDBOX.md.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Sandbox is a host-local sandbox confined to a single root directory.
type Sandbox struct {
	root     string // absolute, cleaned — the lexical fence boundary.
	realRoot string // root with all symlinks resolved — the symlink-safe boundary.

	// alias is an OPTIONAL absolute prefix that maps to the root — the in-container
	// mount point (e.g. "/workspace") when this backend is composed under docker.
	// The model lives in the container, so it naturally writes "/workspace/f.go";
	// the fence is host-side and root-relative, so without the alias that path is
	// refused as off-root and the model's mental model fights the tools
	// (DUET-DOGFOOD F4). resolve() strips the alias before fencing — no widening:
	// the stripped path goes through the exact same lexical+symlink checks.
	alias string

	// procMu guards procs: the live StartProcess children. The local backend has no
	// container to reap on teardown (unlike docker's `rm -f`), so Close must kill any
	// the caller forgot — else a crashed/abandoned run leaks host processes.
	procMu sync.Mutex
	procs  []sandbox.Process
}

// compile-time proof we satisfy the interface(s).
var (
	_ sandbox.Sandbox         = (*Sandbox)(nil)
	_ sandbox.LimitedReader   = (*Sandbox)(nil)
	_ sandbox.Appender        = (*Sandbox)(nil)
	_ sandbox.WorkdirReporter = (*Sandbox)(nil)
)

// New creates a local sandbox rooted at dir. dir must exist and be a directory;
// it is resolved to an absolute, cleaned path that becomes the fence boundary.
func New(dir string) (*Sandbox, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sandbox root %q is not a directory", abs)
	}
	// realRoot is the root with its OWN symlinks resolved, so the escape check
	// below compares like with like (a path's resolved real target against the
	// resolved real root). If the root itself sits under a symlinked dir (e.g.
	// /var -> /private/var), comparing a resolved path against the unresolved root
	// would wrongly reject every legitimate path.
	realRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &Sandbox{root: abs, realRoot: realRoot}, nil
}

// Capabilities reports the truth: no isolation, host network. The runner can use
// this to refuse running untrusted code here.
func (s *Sandbox) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Isolation: sandbox.IsolationNone, Network: true}
}

// Root returns the absolute, cleaned fence root. It exists so a backend that
// COMPOSES this one — the docker backend embeds *local.Sandbox and must
// bind-mount the same resolved directory into the container (so a host write and
// an in-container read hit the same inode) — can read the path the fence resolved
// to, rather than re-resolving dir itself and risking a mismatch.
func (s *Sandbox) Root() string { return s.root }

// RealRoot returns the fence root with all symlinks resolved. The docker backend
// bind-mounts THIS (not the lexical Root) into the container, so the mounted host
// path is the canonical one the symlink-safe fence checks against — they can't
// disagree, and a symlinked root dir can't later be swapped to redirect the mount.
func (s *Sandbox) RealRoot() string { return s.realRoot }

// RelInRoot fences path exactly as ReadFile/Exec do, then returns it cleaned and
// RELATIVE to the root ("." for the root itself). The docker backend uses this to
// translate a sandbox-relative working dir into an in-container path while reusing
// the tested fence — an escaping path is REFUSED here, not silently clamped, so
// the container's working dir can't be aimed outside the workspace.
func (s *Sandbox) RelInRoot(path string) (string, error) {
	abs, err := s.resolve(path)
	if err != nil {
		return "", err
	}
	return filepath.Rel(s.root, abs)
}

// Workdir reports the directory Exec commands start in, as the model should
// name it (sandbox.WorkdirReporter). Plain host use: the absolute root. When a
// mount alias is set this backend is composed under a container and the model
// lives at the mount point, so the alias IS the model-visible cwd.
func (s *Sandbox) Workdir() string {
	if s.alias != "" {
		return s.alias
	}
	return s.root
}

// SetMountAlias declares an absolute prefix that aliases the root: the
// in-container bind-mount point (e.g. "/workspace") when this backend is composed
// under a container backend. Paths arriving with that prefix are translated to
// root-relative before fencing, so a model that (correctly) believes it lives at
// the mount point can use absolute paths without being refused (DUET-DOGFOOD F4).
// A trailing slash is trimmed; "" disables the alias.
func (s *Sandbox) SetMountAlias(prefix string) {
	s.alias = strings.TrimSuffix(prefix, "/")
}

// resolve maps a sandbox-relative path to an absolute host path, refusing any
// path that escapes the root. This IS the fence, and it has TWO layers:
//
//  1. Lexical: filepath.Join cleans the path; a "../" escape resolves above the
//     root and is rejected by the prefix check. This alone suffices when the
//     workspace contents are trusted (the plain local backend).
//
//  2. Symlink-safe: when this backend is COMPOSED under the docker backend, the
//     workspace is bind-mounted into a container where HOSTILE code runs, and
//     that code can plant a symlink (e.g. /workspace/leak -> /etc/passwd) to turn
//     a later host-side ReadFile/WriteFile into a confused-deputy escape — the
//     lexical check passes because "leak" is under the root, but os.ReadFile then
//     follows the link off-root. escapesViaSymlink closes that: it resolves the
//     path's real target (and, for a not-yet-existing file, its real parent) and
//     refuses if it lands outside realRoot.
//
// Residual: this is a check-then-use, so a symlink swapped in AFTER resolve but
// BEFORE the os op is a TOCTOU window. Fully closing it needs openat2 with
// RESOLVE_BENEATH (Linux-specific); the EvalSymlinks check closes the practical
// planted-symlink attack and is the documented boundary today.
func (s *Sandbox) resolve(path string) (string, error) {
	if path == "" {
		path = "."
	}
	// Mount-alias translation (DUET-DOGFOOD F4): "/workspace/f.go" from a model
	// living in the container means "<root>/f.go" here. Strip the alias and fall
	// through to the normal fence — the translated path is checked like any other.
	if s.alias != "" {
		if path == s.alias {
			path = "."
		} else if strings.HasPrefix(path, s.alias+"/") {
			path = path[len(s.alias)+1:]
		}
	}
	abs := filepath.Join(s.root, path)
	if abs != s.root && !strings.HasPrefix(abs, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("refused: %q is outside the sandbox root %s", path, s.root)
	}
	if err := s.escapesViaSymlink(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// escapesViaSymlink reports (as an error) whether abs — already lexically inside
// the fence — would resolve through symlinks to a real path OUTSIDE realRoot. It
// resolves the longest existing prefix of abs (the not-yet-existing suffix, e.g.
// the new file in a WriteFile, can't itself be a symlink, so it's safe once its
// real parent is confirmed in-bounds) and checks that real target stays under
// realRoot. Fails closed: an unexpected stat error is treated as a refusal.
func (s *Sandbox) escapesViaSymlink(abs string) error {
	p := abs
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			if real != s.realRoot && !strings.HasPrefix(real, s.realRoot+string(filepath.Separator)) {
				return fmt.Errorf("refused: %q resolves through a symlink to %q, outside the sandbox root", abs, real)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("refused: cannot verify %q is within the sandbox: %w", abs, err)
		}
		// p doesn't exist yet; step up to its parent and try again. The base we
		// skip over is a name that doesn't exist, so it cannot be a symlink.
		parent := filepath.Dir(p)
		if parent == p {
			// Walked to the filesystem root without finding an existing ancestor —
			// impossible once realRoot exists, but fail closed if it happens.
			return fmt.Errorf("refused: %q has no existing ancestor within the sandbox", abs)
		}
		p = parent
	}
}

// Exec runs a command to completion. A non-zero exit (or a Timeout kill) is a
// normal *Result, not an error — only a genuine failure to start the command is
// returned as err (Principle 6: failures are observations).
func (s *Sandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	dir := s.root
	if cmd.Dir != "" {
		d, err := s.resolve(cmd.Dir)
		if err != nil {
			return nil, err
		}
		dir = d
	}

	runCtx := ctx
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(runCtx, cmd.Path, cmd.Args...)
	c.Dir = dir
	c.Env = append(os.Environ(), cmd.Env...)
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	start := time.Now()
	err := c.Run()
	res := &sandbox.Result{
		ExitCode: -1,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: time.Since(start),
	}
	if c.ProcessState != nil {
		res.ExitCode = c.ProcessState.ExitCode()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}

	if err != nil {
		// Non-zero exit (incl. a timeout-killed process) is a Result the caller
		// observes — not a Go error. Only "couldn't run it at all" is an error.
		var ee *exec.ExitError
		if errors.As(err, &ee) || res.TimedOut {
			return res, nil
		}
		return res, fmt.Errorf("exec %q: %w", cmd.Path, err)
	}
	return res, nil
}

// ReadFile reads a file within the fence.
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	abs, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// ReadFileLimit implements sandbox.LimitedReader: a bounded read that never pulls
// more than max bytes into memory (Principle 4). It reads max+1 bytes — one past
// the limit — to detect whether the file continued, without copying the tail.
func (s *Sandbox) ReadFileLimit(ctx context.Context, path string, max int64) ([]byte, bool, error) {
	abs, err := s.resolve(path)
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if max <= 0 {
		data, err := io.ReadAll(f) // "no limit" == ReadFile.
		return data, false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// WriteFile writes a file within the fence (parent directories must already exist).
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	abs, err := s.resolve(path)
	if err != nil {
		return err
	}
	return os.WriteFile(abs, data, mode)
}

// AppendFile implements sandbox.Appender: shell `>>` semantics inside the fence
// — data lands at the end of the file (created with mode if missing) without the
// existing contents ever entering memory.
func (s *Sandbox) AppendFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	abs, err := s.resolve(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_APPEND, mode)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// ListDir lists a directory within the fence.
func (s *Sandbox) ListDir(ctx context.Context, path string) ([]sandbox.DirEntry, error) {
	abs, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, sandbox.DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

// Close releases resources. For plain Exec the local backend holds none; but any
// long-lived StartProcess children are killed here so an abandoned run can't leak
// host processes (the local counterpart to docker's `rm -f`). Callers should still
// Kill processes they own; this is the backstop.
func (s *Sandbox) Close() error {
	s.procMu.Lock()
	procs := s.procs
	s.procs = nil
	s.procMu.Unlock()
	for _, p := range procs {
		_ = p.Kill()
	}
	return nil
}

// trackProcess records a live process so Close can reap it. Returns p for chaining.
func (s *Sandbox) trackProcess(p sandbox.Process) sandbox.Process {
	s.procMu.Lock()
	s.procs = append(s.procs, p)
	s.procMu.Unlock()
	return p
}
