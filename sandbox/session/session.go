// Package session implements sandbox.Session — a stateful execution context in
// which successive Exec calls share working directory and environment — once, for
// ANY sandbox.Sandbox backend. The docker and local backends both construct one of
// these (differing only in the state-dir path), so the stateful-shell logic lives
// in exactly one place.
//
// The mechanism (see ../../SESSION.md for the decided design): the underlying
// Exec is stateless — a fresh process at the default cwd with the base env every
// call. To carry cwd + env across calls WITHOUT a long-lived shell process (that
// is the ProcessHost slice, deliberately deferred), each shell command is wrapped:
//
//	restore  source a per-session env file; cd to the saved cwd
//	run      eval the model's command IN this shell (so its cd/export take effect)
//	capture  write the new cwd and `export -p` back to the state file
//
// The state file lives in a writable, container-lifetime directory the backend
// chooses (the docker /tmp tmpfs; the host temp dir for local). Only shell `-c`
// commands are wrapped — a raw process (rg, git) can't mutate session state, so it
// passes straight through.
package session

import (
	"context"
	"strings"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Session is a stateful view over an underlying Sandbox. It is NOT safe for
// concurrent Exec calls (the agent loop is sequential); two independent Sessions
// over the same backend are isolated by their distinct state dirs.
type Session struct {
	sb       sandbox.Sandbox // the backend whose stateless Exec we drive.
	stateDir string          // per-session dir holding the env + cwd state files.
}

var _ sandbox.Session = (*Session)(nil)

// New returns a Session that drives sb's Exec, persisting shell state under
// stateDir. stateDir must be a writable path that survives across Exec calls for
// the backend (e.g. the docker container's /tmp tmpfs); the backend owns choosing
// it and making it unique per Session. The dir is created lazily on first Exec.
func New(sb sandbox.Sandbox, stateDir string) *Session {
	return &Session{sb: sb, stateDir: stateDir}
}

// Exec runs cmd with the session's accumulated cwd + env. A shell `-c` command is
// wrapped so its `cd`/`export` persist to the next call; anything else passes
// through to the backend unchanged (a single process carries no session state).
// The contract otherwise matches sandbox.Exec exactly (non-zero exit is a Result,
// not an error).
func (s *Session) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	body, ok := shellBody(cmd)
	if !ok {
		return s.sb.Exec(ctx, cmd) // non-shell: no state to carry.
	}

	// With an explicit Dir the backend's -w puts us there; don't also restore the
	// saved cwd (the explicit Dir wins and, via the post-command pwd capture,
	// becomes the new session cwd). With no Dir, restore the saved cwd.
	restore := "1"
	if cmd.Dir != "" {
		restore = "0"
	}

	wrapped := sandbox.Command{
		Path:    "sh",
		Args:    []string{"-c", wrapperScript},
		Dir:     cmd.Dir,
		Stdin:   cmd.Stdin,
		Timeout: cmd.Timeout,
		Env:     append([]string(nil), cmd.Env...), // copy so we never mutate caller's slice.
	}
	// Pass the command body + state-dir + restore flag via ENV, never interpolated
	// into the script — the script is a static constant, so the model's command can
	// carry no shell-injection into the wrapper itself. eval re-parses the body as a
	// command, which is exactly what `sh -c <body>` would have done.
	wrapped.Env = append(wrapped.Env,
		"__SESSION_ST="+s.stateDir,
		"__SESSION_RESTORE="+restore,
		"__SESSION_BODY="+body,
	)
	return s.sb.Exec(ctx, wrapped)
}

// Close removes the session's state files. Best-effort: the backend tears the
// whole container (and its /tmp) down on its own Close, so a failure here only
// leaks a few bytes for the rest of the run, not across runs.
func (s *Session) Close() error {
	_, err := s.sb.Exec(context.Background(), sandbox.Command{
		Path: "sh",
		Args: []string{"-c", `rm -rf -- "$__SESSION_ST"`},
		Env:  []string{"__SESSION_ST=" + s.stateDir},
	})
	return err
}

// shellBody returns the command string of a shell `-c` invocation (the form the
// run tool builds: `sh -c <string>`), and false for anything else. It mirrors
// gated.Display's recognition of the same shape.
func shellBody(cmd sandbox.Command) (string, bool) {
	base := cmd.Path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if (base == "sh" || base == "bash") && len(cmd.Args) == 2 && cmd.Args[0] == "-c" {
		return cmd.Args[1], true
	}
	return "", false
}

// wrapperScript is the static stateful-shell wrapper. It restores env + cwd, runs
// the model's command in THIS shell via eval (so cd/export mutate it and are
// captured), then writes the new cwd and full exported env back. Every restore
// step is best-effort (2>/dev/null, no `set -e`): a missing state file, a vanished
// saved cwd, or a re-export of a read-only var must never abort the command. POSIX
// only — the container shell is busybox ash, the local shell is the host /bin/sh.
const wrapperScript = `mkdir -p "$__SESSION_ST" 2>/dev/null
[ -f "$__SESSION_ST/env" ] && . "$__SESSION_ST/env" 2>/dev/null
if [ "$__SESSION_RESTORE" = 1 ] && [ -f "$__SESSION_ST/cwd" ]; then
  cd "$(cat "$__SESSION_ST/cwd")" 2>/dev/null
fi
eval "$__SESSION_BODY"
__session_rc=$?
__session_envfile="$__SESSION_ST/env"
pwd > "$__SESSION_ST/cwd" 2>/dev/null
unset __SESSION_BODY __SESSION_RESTORE __SESSION_ST
export -p > "$__session_envfile" 2>/dev/null
exit $__session_rc`
