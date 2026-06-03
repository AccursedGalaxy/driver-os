package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/proc"
)

// The local backend is a ProcessHost: it can start a long-lived host child and
// hold its streams open (see ../../SESSION.md slice 2). IsolationNone still applies
// — this is for TRUSTED long-lived tools (a language server we ship), not
// model-authored code.
var _ sandbox.ProcessHost = (*Sandbox)(nil)

// StartProcess launches cmd as a long-lived child confined to the workspace dir
// (cwd fenced like Exec). Streams are wired with explicit os.Pipe and caller-owned
// ends so reaping the process never races an in-flight reader (the os/exec
// StdoutPipe/Wait hazard). The child runs in its OWN process group so Kill can take
// down any grandchildren it spawned. cmd.Stdin/cmd.Timeout are ignored (P: live
// stdin + caller-managed lifetime).
func (s *Sandbox) StartProcess(ctx context.Context, cmd sandbox.Command) (sandbox.Process, error) {
	if cmd.Path == "" {
		return nil, fmt.Errorf("local StartProcess: empty command path")
	}
	dir := s.root
	if cmd.Dir != "" {
		d, err := s.resolve(cmd.Dir)
		if err != nil {
			return nil, err // escaping Dir refused, same as Exec.
		}
		dir = d
	}

	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("local StartProcess: stdin pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("local StartProcess: stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("local StartProcess: stderr pipe: %w", err)
	}

	c := exec.Command(cmd.Path, cmd.Args...)
	c.Dir = dir
	c.Env = append(os.Environ(), cmd.Env...)
	c.Stdin, c.Stdout, c.Stderr = inR, outW, errW
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group → Kill reaps children too.

	if err := c.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW, errR, errW} {
			f.Close()
		}
		return nil, fmt.Errorf("local StartProcess: %w", err)
	}
	// The parent closes the CHILD ends so EOF propagates: closing our inW gives the
	// child stdin EOF; the child's exit closes its outW/errW so our outR/errR see EOF.
	inR.Close()
	outW.Close()
	errW.Close()

	pgid := c.Process.Pid // == the group id, since Setpgid made the child a leader.

	var killOnce sync.Once
	kill := func() error {
		killOnce.Do(func() {
			_ = inW.Close()                          // EOF to stdin (lets a well-behaved process exit)...
			_ = syscall.Kill(-pgid, syscall.SIGKILL) // ...and SIGKILL the whole group regardless.
		})
		return nil
	}

	p := proc.New(inW, outR, errR, proc.DefaultStderrCap, c.Wait, kill)
	s.trackProcess(p)

	// Reap on ctx cancellation; stop watching once the process exits on its own (so
	// the goroutine never outlives the process).
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.Done():
		}
	}()

	return p, nil
}
