// Package gated wraps any sandbox.Sandbox with a confirmation gate on command
// execution — the trust layer that makes the agent usable on a REAL repo rather
// than only a throwaway eval fixture. It is a decorator, not a change to the
// loop: every effect the agent causes already flows through the sandbox.Sandbox
// interface (sandbox/sandbox.go), so gating `run` is just wrapping Exec. The
// loop and the tools never learn the gate exists.
//
// Scope is deliberately Exec only. File writes route through WriteFile, which is
// already path-confined to the sandbox root AND reviewed as a diff before any
// commit (the cmd/agent -review flow), so gating them in-loop would add prompts
// without adding safety. Arbitrary shell — which on the local backend runs on
// the host (IsolationNone) — is the real escape surface, so that is what the
// gate guards.
//
// A blocked command is NOT an error: it returns a non-zero Result the model sees
// as an observation and can adapt to (Principle 6 — tool failures are
// observations, not crashes), exactly like a command that exited non-zero on its
// own. The agent simply learns "that wasn't allowed" and tries another way.
package gated

import (
	"context"
	"io/fs"
	"strings"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Verdict is a Policy's classification of a command before it runs. The zero
// value is Ask on purpose: a policy that doesn't recognize a command falls
// through to a human, so an unanticipated command is never silently allowed.
type Verdict int

const (
	Ask   Verdict = iota // unsure — defer to the Approver (the safe default).
	Allow                // clearly safe (read/build/test) — run without asking.
	Deny                 // never run — and don't bother asking.
)

// Policy classifies a command into a Verdict. It is pure (no I/O): the human
// interaction lives in the Approver, so a Policy stays trivially testable and
// swappable (tighten it to AskAll, or loosen it per project).
type Policy func(sandbox.Command) Verdict

// Request is what an Approver is asked to approve: the raw command plus a
// human-readable rendering of it (Display) so the prompt doesn't have to know
// the sh -c wrapping the run tool uses.
type Request struct {
	Command sandbox.Command
	Display string
}

// Approver decides an Ask verdict. Approve returns true to run the command. An
// error (e.g. no interactive terminal to prompt on) is treated by the gate as a
// denial — failing closed, never open.
type Approver interface {
	Approve(ctx context.Context, req Request) (bool, error)
}

// ApproverFunc adapts a plain func to an Approver.
type ApproverFunc func(context.Context, Request) (bool, error)

// Approve calls f.
func (f ApproverFunc) Approve(ctx context.Context, r Request) (bool, error) { return f(ctx, r) }

// Sandbox decorates an inner sandbox.Sandbox, gating Exec through a Policy and
// (on Ask) an Approver. Every other method passes straight through — the gate
// adds nothing to reads, writes, listing, capabilities, or teardown.
type Sandbox struct {
	inner    sandbox.Sandbox
	policy   Policy
	approver Approver
}

// New wraps inner with a confirmation gate. A nil policy defaults to
// DefaultPolicy; a nil approver makes every Ask a denial (a fully un-attended
// gate that blocks anything not explicitly Allowed — fail closed).
func New(inner sandbox.Sandbox, approver Approver, policy Policy) *Sandbox {
	if policy == nil {
		policy = DefaultPolicy
	}
	return &Sandbox{inner: inner, policy: policy, approver: approver}
}

// Exec gates the command. Allow runs it; Deny blocks it; Ask consults the
// Approver. A blocked command returns a synthetic non-zero Result (never a Go
// error), so the agent observes the denial and continues (P6).
func (s *Sandbox) Exec(ctx context.Context, cmd sandbox.Command) (*sandbox.Result, error) {
	switch s.policy(cmd) {
	case Allow:
		return s.inner.Exec(ctx, cmd)
	case Deny:
		return blocked(cmd, "blocked by policy"), nil
	default: // Ask
		if s.approver == nil {
			return blocked(cmd, "blocked: no approver to confirm with"), nil
		}
		ok, err := s.approver.Approve(ctx, Request{Command: cmd, Display: Display(cmd)})
		if err != nil {
			return blocked(cmd, "blocked: approval unavailable ("+err.Error()+")"), nil
		}
		if !ok {
			return blocked(cmd, "blocked: denied by user"), nil
		}
		return s.inner.Exec(ctx, cmd)
	}
}

// Capabilities, ReadFile, WriteFile, ListDir, Close all pass through unchanged.
func (s *Sandbox) Capabilities() sandbox.Capabilities { return s.inner.Capabilities() }
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.inner.ReadFile(ctx, path)
}
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	return s.inner.WriteFile(ctx, path, data, mode)
}
func (s *Sandbox) ListDir(ctx context.Context, path string) ([]sandbox.DirEntry, error) {
	return s.inner.ListDir(ctx, path)
}
func (s *Sandbox) Close() error { return s.inner.Close() }

// blocked is the observation a gated-off command yields: exit 126 (the shell's
// "found but not executable/permitted" code — apt for "refused"), with the
// reason on stderr where formatRun surfaces it to the model.
func blocked(cmd sandbox.Command, why string) *sandbox.Result {
	return &sandbox.Result{ExitCode: 126, Stderr: []byte(why + ": " + Display(cmd))}
}

// Display renders a Command as the human would read it. The run tool always
// shells `sh -c "<command>"`, so the meaningful line is the -c argument — show
// that, not the wrapper. Anything else is shown as Path + Args.
func Display(cmd sandbox.Command) string {
	if (cmd.Path == "sh" || cmd.Path == "bash") && len(cmd.Args) == 2 && cmd.Args[0] == "-c" {
		return cmd.Args[1]
	}
	if len(cmd.Args) == 0 {
		return cmd.Path
	}
	return cmd.Path + " " + strings.Join(cmd.Args, " ")
}

// safePrefixes are command lines DefaultPolicy auto-allows: read-only
// inspection, builds, and tests — the bulk of an agent's traffic, none of which
// mutates state outside the build cache. It is a HEURISTIC allowlist, not a
// security boundary (a security boundary is the sandbox isolation level, not a
// string match): its job is to keep the gate quiet on the obvious-safe commands
// so a human's attention is spent on the rest. Tune it per project.
var safePrefixes = []string{
	"go test", "go build", "go vet", "go doc", "go list", "go env", "gofmt",
	"ls", "cat", "head", "tail", "wc", "pwd", "echo", "find", "tree", "stat",
	"grep", "rg", "ag", "which", "file", "diff",
	"git status", "git diff", "git log", "git show", "git branch", "git rev-parse",
}

// DefaultPolicy auto-allows the read/build/test allowlist and asks for
// everything else. It never returns Deny — a human can approve anything; the
// policy only decides what is quiet enough to skip the prompt. An unrecognized
// command falls through to Ask (the zero Verdict), which is the fail-safe.
//
// A line with shell chaining or redirection (`&&`, `;`, `|`, `>`, `$(`, …) is
// ALWAYS Ask even if it starts with a safe prefix: "go test ./... && rm -rf ~"
// must not ride in on "go test". The allowlist only trusts a single, simple
// command — anything that can hide a second one is shown to the human.
func DefaultPolicy(cmd sandbox.Command) Verdict {
	line := strings.TrimSpace(Display(cmd))
	if hasShellChaining(line) {
		return Ask
	}
	for _, p := range safePrefixes {
		if line == p || strings.HasPrefix(line, p+" ") {
			return Allow
		}
	}
	return Ask
}

// hasShellChaining reports whether a command line can run more than the one
// command its prefix suggests — via sequencing, piping, redirection, command
// substitution, or a newline. Such a line is never auto-allowed.
func hasShellChaining(line string) bool {
	for _, tok := range []string{"&&", "||", ";", "|", ">", "<", "`", "$(", "\n", "&"} {
		if strings.Contains(line, tok) {
			return true
		}
	}
	return false
}

// AskAll is the strictest policy: every command is Ask. Use it when even reads
// should be confirmed (an untrusted task, a shared machine).
func AskAll(sandbox.Command) Verdict { return Ask }
