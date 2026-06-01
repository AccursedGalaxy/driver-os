// Package sandbox defines the isolation boundary the agent acts inside.
//
// Motivation (see ../SANDBOX.md). Today the agent runs OUR trusted tools behind
// a path fence (confineToRoot in cmd/agent). The moment the agent runs
// arbitrary, model-authored code, that code is hostile and a path check is not
// enough. Rather than scatter isolation logic through individual tools, we make
// it a property of ONE boundary: every effect the model causes — running a
// command, reading or writing a file — goes through a Sandbox.
//
// This is the same shape as the rest of driver-os: a small stable core
// interface (like llm.Provider) with pluggable backend adapters in subpackages
// (like provider/openaicompat). The backends form an isolation spectrum, all
// behind this one interface so the agent code never changes when we graduate:
//
//	sandbox/local       subprocess + path fence   IsolationNone     (today)
//	sandbox/docker      container, shared kernel   IsolationProcess  (first exec tool)
//	  ^ same backend run with --runtime=runsc      IsolationKernel   (gVisor, one flag)
//	sandbox/firecracker microVM, hardware-virt     IsolationVM       (hostile at scale)
//
// The core package has ZERO external dependencies, on purpose — adapters pull
// their own (Docker SDK, firecracker-go-sdk); the interface stays clean.
package sandbox

import (
	"context"
	"io/fs"
	"time"
)

// Sandbox is a single isolated environment the agent acts inside. One run of the
// agent holds one Sandbox; lifecycle is create (a backend constructor) -> Exec
// and file ops, any number of times -> Close.
//
// The interface is deliberately small. If it can be implemented trivially by the
// local subprocess backend AND faithfully by a Firecracker microVM, it's the
// right size. Richer, backend-specific powers (filesystem snapshots, long-lived
// interactive sessions, streamed output) are intentionally NOT here — they
// belong on optional interfaces discovered via type-assertion, the same way
// llm.Embedder is promoted off llm.Provider. Add them when a backend needs them.
type Sandbox interface {
	// Capabilities advertises what this sandbox actually guarantees, so the
	// agent/runner can enforce policy — e.g. "a tool that runs untrusted code
	// requires Isolation >= IsolationKernel". Never assume; ask.
	Capabilities() Capabilities

	// Exec runs a command to completion inside the sandbox and returns its
	// Result.
	//
	// A non-zero exit code is NOT a Go error — it is a normal Result (Principle
	// 6: tool failures are observations, not crashes; the model sees the stderr
	// and exit code and can correct next turn). err is reserved for the sandbox
	// machinery itself failing: it couldn't start the process, the context was
	// canceled, the container died, etc.
	Exec(ctx context.Context, cmd Command) (*Result, error)

	// ReadFile, WriteFile and ListDir operate on the sandbox's filesystem,
	// confined to its root. Paths are sandbox-relative ("." is the root); a path
	// that escapes the root must be refused by the backend, not honored. For the
	// local backend this is exactly today's confineToRoot, lifted up so every
	// backend enforces the same boundary uniformly.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error
	ListDir(ctx context.Context, path string) ([]DirEntry, error)

	// Close tears the sandbox down and releases its resources. For isolated
	// backends this stops and removes the container/microVM; for the local
	// backend it is effectively a no-op. Always Close (defer it) — an unclosed
	// microVM is a leaked VM.
	Close() error
}

// Command describes one execution request. It mirrors os/exec idioms so the
// local backend is a thin wrapper, while staying serializable so remote backends
// (container, VM) can ship it across the boundary.
type Command struct {
	Path  string   // program to run (or a shell builtin, backend-dependent)
	Args  []string // arguments, NOT including Path
	Dir   string   // working directory, sandbox-relative; "" means the root
	Env   []string // extra environment, each "KEY=VALUE"; merged over the base env
	Stdin []byte   // fed to the process's stdin (e.g. a script piped to an interpreter)

	// Timeout, if > 0, bounds the run independently of ctx. The backend MUST
	// also honor ctx cancellation. Whichever fires first wins. A runaway
	// `while true` is the sandbox's problem to kill, not the agent loop's.
	Timeout time.Duration
}

// Result is the outcome of an Exec. Latency is first-class in this project
// (measured at the Runner layer for model calls); we surface it here too because
// "how long did the model's command take" is a real signal.
type Result struct {
	ExitCode int           // process exit status; 0 == success by convention
	Stdout   []byte        // captured standard output
	Stderr   []byte        // captured standard error
	Duration time.Duration // wall-clock time the command ran
	TimedOut bool          // true if the command was killed by Timeout/ctx
}

// DirEntry is one entry from ListDir. It is a flat value, not fs.DirEntry,
// because that interface (Info/Type) can't be satisfied cheaply across a VM
// boundary — and ListDir's only real consumers need just the name and kind.
type DirEntry struct {
	Name  string
	IsDir bool
}

// Capabilities is what a Sandbox guarantees about itself. It maps directly onto
// the isolation spectrum in SANDBOX.md, encoding the threat-model decision into
// the type system rather than leaving it implicit.
type Capabilities struct {
	Isolation Isolation // the strength of the boundary
	Network   bool      // whether code inside can reach the network
}

// Isolation is how strongly a backend separates the executed code from the host.
// Ordered weakest -> strongest so policy checks can use simple comparison
// (require >= IsolationKernel before running untrusted code).
type Isolation int

const (
	// IsolationNone: runs as a child process on the host, confined only by a
	// path fence. Fine ONLY for code we wrote and trust. (sandbox/local)
	IsolationNone Isolation = iota
	// IsolationProcess: a container — separate namespaces/cgroups but a SHARED
	// host kernel. Better, but a kernel exploit escapes. (sandbox/docker)
	IsolationProcess
	// IsolationKernel: gVisor — syscalls hit a userspace application kernel, not
	// the host kernel. Strong, container-speed. (docker + --runtime=runsc)
	IsolationKernel
	// IsolationVM: a Firecracker microVM — hardware-virtualized, its own kernel.
	// Strongest; needs KVM. For genuinely hostile, multi-tenant code. (sandbox/firecracker)
	IsolationVM
)

// String renders an Isolation level for logs and policy errors.
func (i Isolation) String() string {
	switch i {
	case IsolationNone:
		return "none"
	case IsolationProcess:
		return "process"
	case IsolationKernel:
		return "kernel"
	case IsolationVM:
		return "vm"
	default:
		return "unknown"
	}
}

// ---- optional capabilities (discovered via type-assertion, like llm.Embedder) ----

// LimitedReader is an OPTIONAL capability: a bounded ReadFile that never loads
// more than max bytes into memory. The core ReadFile reads the whole file by
// contract, which is an OOM waiting to happen for a tool pointed at a multi-
// gigabyte path (Principle 4: bound everything — a tool that explodes freezes the
// loop). A caller that would rather cap than trust the file's size discovers this
// the same way as Sessioner:
//
//	if lr, ok := sb.(sandbox.LimitedReader); ok {
//		data, truncated, err := lr.ReadFileLimit(ctx, path, 1<<20)
//	}
type LimitedReader interface {
	// ReadFileLimit reads up to max bytes of path, confined to the sandbox root
	// like ReadFile. If the file is longer it returns exactly the first max bytes
	// with truncated=true and does NOT read the rest into memory. max <= 0 means
	// "no limit", equivalent to ReadFile.
	ReadFileLimit(ctx context.Context, path string, max int64) (data []byte, truncated bool, err error)
}

// Sessioner is an OPTIONAL capability a backend may add: opening a stateful
// Session in which successive Exec calls share process state — working
// directory, environment, and (for a real shell/REPL backend) defined variables
// and background processes. Not every backend supports it; discover it:
//
//	if sr, ok := sb.(sandbox.Sessioner); ok {
//		sess, err := sr.NewSession(ctx)
//		// ... sess.Exec(...) calls now share state ...
//	}
//
// PLANNED, not yet implemented. The local backend treats every Exec as
// independent (fresh process, no shared cwd/env). The first session backend will
// hold a long-lived shell so `export X=1` on one turn is visible to `echo $X` on
// the next — the stateful counterpart to the stateless core. (Confirmed wanted.)
type Sessioner interface {
	NewSession(ctx context.Context) (Session, error)
}

// Session is a stateful execution context. Exec calls within one Session observe
// the effects of earlier ones (cwd changes, env, shell state). Always Close it.
type Session interface {
	Exec(ctx context.Context, cmd Command) (*Result, error)
	Close() error
}
