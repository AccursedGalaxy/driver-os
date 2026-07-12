package headless

import (
	"context"
	"io/fs"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// fakeBase is a minimal sandbox.Sandbox WITHOUT the optional LimitedReader
// capability, so tests can exercise sessionSandbox's degraded fallback.
type fakeBase struct {
	readData []byte
}

func (f *fakeBase) Exec(context.Context, sandbox.Command) (*sandbox.Result, error) {
	return &sandbox.Result{}, nil
}
func (f *fakeBase) Capabilities() sandbox.Capabilities                           { return sandbox.Capabilities{} }
func (f *fakeBase) ReadFile(context.Context, string) ([]byte, error)             { return f.readData, nil }
func (f *fakeBase) WriteFile(context.Context, string, []byte, fs.FileMode) error { return nil }
func (f *fakeBase) Remove(context.Context, string) error                         { return nil }
func (f *fakeBase) ListDir(context.Context, string) ([]sandbox.DirEntry, error)  { return nil, nil }
func (f *fakeBase) Close() error                                                 { return nil }

// fakeRootedBase adds a workspace root, like the local backend.
type fakeRootedBase struct {
	fakeBase
	root string
}

func (f *fakeRootedBase) Root() string { return f.root }

func TestSessionSandboxReadFileLimitFallbackCapsKeptBytes(t *testing.T) {
	// A base without LimitedReader degrades to full-read-then-cap; the adapter
	// must never hand the caller more than max (the P4 keep-bound).
	s := sessionSandbox{Sandbox: &fakeBase{readData: make([]byte, 100)}}
	data, trunc, err := s.ReadFileLimit(context.Background(), "big.txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 10 || !trunc {
		t.Errorf("fallback must cap kept bytes at max: len=%d trunc=%v, want 10/true", len(data), trunc)
	}
	data, trunc, err = s.ReadFileLimit(context.Background(), "big.txt", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 100 || trunc {
		t.Errorf("an under-cap file must pass through whole: len=%d trunc=%v, want 100/false", len(data), trunc)
	}
}

func TestSessionSandboxRootForwards(t *testing.T) {
	s := sessionSandbox{Sandbox: &fakeRootedBase{root: "/work"}}
	if got := s.Root(); got != "/work" {
		t.Errorf("Root() = %q, want the base root forwarded", got)
	}
	if got := (sessionSandbox{Sandbox: &fakeBase{}}).Root(); got != "" {
		t.Errorf("Root() on a rootless base = %q, want \"\"", got)
	}
}

// fakeWorkdirBase adds a workdir, like the local backend.
type fakeWorkdirBase struct {
	fakeBase
	wd string
}

func (f *fakeWorkdirBase) Workdir() string { return f.wd }

func TestSessionSandboxWorkdirForwards(t *testing.T) {
	s := sessionSandbox{Sandbox: &fakeWorkdirBase{wd: "/home/user"}}
	var wr sandbox.WorkdirReporter = s // the capability must stay visible through the adapter.
	if got := wr.Workdir(); got != "/home/user" {
		t.Errorf("Workdir() = %q, want the base workdir forwarded", got)
	}
	if got := (sessionSandbox{Sandbox: &fakeBase{}}).Workdir(); got != "" {
		t.Errorf("Workdir() on a workdir-less base = %q, want \"\"", got)
	}
}
