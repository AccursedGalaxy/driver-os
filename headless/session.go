package headless

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// sessionSandbox routes the model's shell `run` commands through a stateful
// Session (cwd/env persist across commands) while delegating everything else —
// file ops, Capabilities, Close — to the base Sandbox it embeds. It is the -session
// adapter, sitting BELOW the review gate so the gate inspects the real command, not
// the session's shell wrapper (see docs/specs/SESSION.md).
type sessionSandbox struct {
	sandbox.Sandbox                 // base: file ops, Capabilities, Close.
	sess            sandbox.Session // stateful exec; wraps shell commands, passes others through.
}

var (
	_ sandbox.Sandbox         = sessionSandbox{}
	_ sandbox.LimitedReader   = sessionSandbox{}
	_ sandbox.Appender        = sessionSandbox{}
	_ sandbox.WorkdirReporter = sessionSandbox{}
)

// Exec routes through the session. The session itself decides whether to wrap the
// command (a shell `-c`) for state or pass it straight to the base (anything else).
func (s sessionSandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	return s.sess.Exec(ctx, cmd)
}

// ReadFileLimit forwards the optional bounded-read capability when the base has it
// (the docker and local backends do), so wrapping in this adapter doesn't silently
// drop the P4 memory bound the read tool relies on. Without the capability it
// degrades the way agent.readBounded's own fallback does: full read, then cap what
// is KEPT — otherwise wrapping would return more than max to the caller.
func (s sessionSandbox) ReadFileLimit(ctx context.Context, path string, max int64) ([]byte, bool, error) {
	if lr, ok := s.Sandbox.(sandbox.LimitedReader); ok {
		return lr.ReadFileLimit(ctx, path, max)
	}
	data, err := s.Sandbox.ReadFile(ctx, path)
	if err != nil {
		return nil, false, err
	}
	if max >= 0 && int64(len(data)) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// AppendFile forwards the optional sandbox.Appender capability. A base without
// it gets errors.ErrUnsupported so agent.writeFileOp keeps one policy point for
// the degraded read+concat path.
func (s sessionSandbox) AppendFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	if ap, ok := s.Sandbox.(sandbox.Appender); ok {
		return ap.AppendFile(ctx, path, data, mode)
	}
	return fmt.Errorf("append %s: %w", path, errors.ErrUnsupported)
}

// Root forwards the base sandbox's workspace root when it exposes one, so
// host-side Go introspection (go doc / go list) stays exact under -session.
func (s sessionSandbox) Root() string {
	if r, ok := s.Sandbox.(interface{ Root() string }); ok {
		return r.Root()
	}
	return ""
}

// Workdir forwards the optional sandbox.WorkdirReporter capability.
func (s sessionSandbox) Workdir() string {
	if wr, ok := s.Sandbox.(sandbox.WorkdirReporter); ok {
		return wr.Workdir()
	}
	return ""
}
