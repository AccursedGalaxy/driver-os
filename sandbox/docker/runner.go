package docker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
)

// currentUserSpec returns the host user as "uid:gid". Running the container as
// this user (the default for Options.User) keeps the process non-root AND makes
// files it writes to the bind mount owned by the host user, so host-side file ops
// can read them back (the uid-mismatch gotcha, §13).
func currentUserSpec() string {
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}

// dockerRunner is the one-method seam between the backend and the `docker` CLI.
// Everything the backend does to a container — run, exec, kill, rm — is one
// invocation of the binary with an argv and optional stdin. Keeping it a single
// interface method means tests inject a fake and NEVER need a daemon (Slice 1's
// whole point), and the real impl stays a thin os/exec wrapper.
//
// Run reports the command's own exit code in `exit` and reserves `err` for the
// machinery failing to run the binary at all (binary not found, context
// cancelled) — the same split sandbox.Exec makes between a non-zero Result and a
// Go error. A non-zero `exit` with nil `err` is a normal outcome the caller
// classifies (e.g. `docker exec` returning the inner command's exit code).
type dockerRunner interface {
	Run(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, exit int, err error)
}

// execRunner is the production dockerRunner: it shells out to the configured
// binary (docker or podman). It holds the binary name so the Podman story is just
// "construct it with podman" — no other code changes.
type execRunner struct {
	binary    string
	maxOutput int64 // per-stream cap on retained output; <=0 means unlimited.
}

// Run executes `<binary> <args...>`, feeding stdin if non-empty, and returns the
// captured streams plus exit code. A non-zero exit (an *exec.ExitError) is NOT an
// err — it's reported via `exit`, matching the dockerRunner contract. Only a
// genuine failure to launch the binary (or ctx cancellation) is an err.
func (r execRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	c := exec.CommandContext(ctx, r.binary, args...)
	if len(stdin) > 0 {
		c.Stdin = bytes.NewReader(stdin)
	}
	// Cap retained output so a flooding command can't OOM the agent (P4). The
	// capped writers keep DRAINING past the limit (so the process never blocks on a
	// full pipe) but stop storing — partial output is preserved, not the deluge.
	stdout := &cappedBuffer{max: r.maxOutput}
	stderr := &cappedBuffer{max: r.maxOutput}
	c.Stdout = stdout
	c.Stderr = stderr

	err := c.Run()
	exitCode := 0
	if c.ProcessState != nil {
		exitCode = c.ProcessState.ExitCode()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// The binary ran and exited non-zero — a normal result, not an error.
			return stdout.Bytes(), stderr.Bytes(), exitCode, nil
		}
		// Couldn't launch it at all (not found, ctx cancelled, …).
		return stdout.Bytes(), stderr.Bytes(), exitCode, err
	}
	return stdout.Bytes(), stderr.Bytes(), exitCode, nil
}

// cappedBuffer is an io.Writer that retains at most max bytes but always reports
// the full length as written — so it bounds memory while os/exec keeps DRAINING
// the pipe (a short write would be the thing that stalls the writer). Bytes past
// the cap are discarded. max <= 0 means unlimited.
type cappedBuffer struct {
	max int64
	buf bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 {
		return b.buf.Write(p)
	}
	if room := b.max - int64(b.buf.Len()); room > 0 {
		keep := p
		if int64(len(keep)) > room {
			keep = keep[:room]
		}
		b.buf.Write(keep)
	}
	return len(p), nil // claim the whole write; the surplus is intentionally dropped.
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }
