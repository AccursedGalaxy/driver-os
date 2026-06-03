package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/proc"
)

// The docker backend is a ProcessHost: it starts a long-lived process INSIDE the
// running container via `docker exec -i` and holds its streams open (see
// ../../SESSION.md slice 2). This is the host the persistent gopls tenant will run
// on, behind the same isolation as Exec.
var _ sandbox.ProcessHost = (*Sandbox)(nil)

var dockerProcCounter atomic.Uint64

// procWrapper is launched by `docker exec`. It records the in-container pid to a
// file, then `exec`s the real program — because `exec` REPLACES the shell, $$ (the
// shell's pid) becomes the program's pid, so the recorded pid is the one Kill must
// signal. This is the crux of reliable teardown: killing the local `docker exec`
// CLIENT does NOT kill the in-container process (it orphans it), so we must signal
// the real pid with a second `docker exec ... kill`.
const procWrapper = `echo $$ > "$__PROC_PIDFILE"; exec "$@"`

// StartProcess launches cmd as a long-lived process in the container. Streams are
// the `docker exec -i` client's stdio (NOT -t: a TTY would cook and merge the
// streams, corrupting framed binary protocols like LSP). cmd.Stdin/cmd.Timeout are
// ignored (live stdin + caller-managed lifetime).
func (s *Sandbox) StartProcess(ctx context.Context, cmd sandbox.Command) (sandbox.Process, error) {
	if cmd.Path == "" {
		return nil, fmt.Errorf("docker StartProcess: empty command path")
	}
	workdir, err := s.containerWorkdir(cmd.Dir) // fenced + translated, same as Exec.
	if err != nil {
		return nil, err
	}

	n := dockerProcCounter.Add(1)
	pidfile := "/tmp/driver-proc-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(n, 10) + ".pid"

	args := []string{"exec", "-i", "-w", workdir, "-e", "__PROC_PIDFILE=" + pidfile}
	for _, e := range s.opts.ExtraEnv { // base env then per-command env (Command.Env wins).
		args = append(args, "-e", e)
	}
	for _, e := range cmd.Env {
		args = append(args, "-e", e)
	}
	args = append(args, s.id, "sh", "-c", procWrapper, "_", cmd.Path)
	args = append(args, cmd.Args...)

	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("docker StartProcess: stdin pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("docker StartProcess: stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("docker StartProcess: stderr pipe: %w", err)
	}

	c := exec.Command(s.opts.Binary, args...)
	c.Stdin, c.Stdout, c.Stderr = inR, outW, errW
	if err := c.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW, errR, errW} {
			f.Close()
		}
		return nil, fmt.Errorf("docker StartProcess: starting `%s exec`: %w", s.opts.Binary, err)
	}
	inR.Close()
	outW.Close()
	errW.Close()

	var killOnce sync.Once
	kill := func() error {
		killOnce.Do(func() {
			// 1. Kill the IN-CONTAINER process by the pid the wrapper recorded — the
			// CLIENT kill below would only orphan it otherwise. Best-effort: a very
			// early Kill may race the pidfile write; the client kill + `rm -f` backstop
			// still reap it.
			if pid := s.readProcPid(pidfile); pid != "" {
				_, _, _, _ = s.runner.Run(context.Background(), []string{"exec", s.id, "kill", "-KILL", pid}, nil)
			}
			// 2. EOF the process's stdin and kill the local `docker exec` client.
			_ = inW.Close()
			if c.Process != nil {
				_ = c.Process.Kill()
			}
			// 3. Drop the pidfile (best-effort hygiene; the container's /tmp is
			// destroyed on the Sandbox's own Close regardless).
			_, _, _, _ = s.runner.Run(context.Background(), []string{"exec", s.id, "rm", "-f", pidfile}, nil)
		})
		return nil
	}

	// Wait on the `docker exec` client: it exits when the in-container process exits,
	// so its exit IS the process's exit (a gopls crash surfaces here).
	p := proc.New(inW, outR, errR, proc.DefaultStderrCap, c.Wait, kill)

	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.Done():
		}
	}()

	return p, nil
}

// readProcPid reads the pid the wrapper recorded for an in-container process,
// trimmed. "" if the file isn't there yet or can't be read.
func (s *Sandbox) readProcPid(pidfile string) string {
	stdout, _, exit, err := s.runner.Run(context.Background(), []string{"exec", s.id, "cat", pidfile}, nil)
	if err != nil || exit != 0 {
		return ""
	}
	return strings.TrimSpace(string(stdout))
}
