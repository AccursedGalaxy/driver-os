package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// fakeRunner records every invocation and returns scripted results, so the unit
// tests assert the exact argv the backend builds and the Result mapping it does —
// with no Docker daemon involved (the whole point of the injectable runner).
type fakeRunner struct {
	calls   [][]string // argv of every Run, in order.
	stdins  [][]byte
	replies []reply // consumed in order; the last one repeats once exhausted.
	n       int
}

type reply struct {
	stdout, stderr string
	exit           int
	err            error
}

func (f *fakeRunner) Run(_ context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, args)
	f.stdins = append(f.stdins, stdin)
	r := reply{stdout: "fake-container-id"} // default: a `run -d` that prints an id.
	if f.n < len(f.replies) {
		r = f.replies[f.n]
	} else if len(f.replies) > 0 {
		r = f.replies[len(f.replies)-1]
	}
	f.n++
	return []byte(r.stdout), []byte(r.stderr), r.exit, r.err
}

// lastCall returns the argv of the most recent Run.
func (f *fakeRunner) lastCall() []string { return f.calls[len(f.calls)-1] }

func newFake(t *testing.T, opts Options, replies ...reply) (*Sandbox, *fakeRunner) {
	t.Helper()
	fr := &fakeRunner{replies: replies}
	sb, err := newWithRunner(t.TempDir(), opts, fr)
	if err != nil {
		t.Fatalf("newWithRunner: %v", err)
	}
	return sb, fr
}

// hasFlagPair reports whether args contains flag immediately followed by value.
func hasFlagPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func has(args []string, tok string) bool {
	for _, a := range args {
		if a == tok {
			return true
		}
	}
	return false
}

func TestRunArgvHasSecurityFlags(t *testing.T) {
	_, fr := newFake(t, Options{})
	run := fr.calls[0] // the `docker run -d ...` from newWithRunner.

	if run[0] != "run" || !has(run, "-d") || !has(run, "--rm") {
		t.Errorf("run argv missing run/-d/--rm: %v", run)
	}
	if !has(run, "--init") {
		t.Error("missing --init (PID 1 must reap zombies, else a pids-limit DoS)")
	}
	if !hasFlagPair(run, "--label", containerLabel+"=1") {
		t.Error("missing --label for leak detection/reaping")
	}
	// §6 hardening checklist — each flag closes a specific escape.
	if !hasFlagPair(run, "--network", "none") {
		t.Error("default must be --network none (no exfiltration)")
	}
	if !hasFlagPair(run, "--cap-drop", "ALL") {
		t.Error("missing --cap-drop ALL")
	}
	if !hasFlagPair(run, "--security-opt", "no-new-privileges") {
		t.Error("missing --security-opt no-new-privileges")
	}
	if !has(run, "--read-only") {
		t.Error("missing --read-only root fs")
	}
	if !hasFlagPair(run, "--tmpfs", "/tmp") {
		t.Error("missing --tmpfs /tmp")
	}
	if !has(run, "--user") {
		t.Error("missing --user (non-root + host-owned writes)")
	}
	if !hasFlagPair(run, "--memory", DefaultMemory) {
		t.Error("missing --memory limit")
	}
	if !hasFlagPair(run, "--cpus", DefaultCPUs) {
		t.Error("missing --cpus limit")
	}
	if !has(run, "--pids-limit") {
		t.Error("missing --pids-limit")
	}
	// The workspace must be the rw mount, mounted at the workdir.
	if !hasFlagPair(run, "-w", DefaultWorkdir) {
		t.Errorf("workdir not set to %s: %v", DefaultWorkdir, run)
	}
	foundMount := false
	for _, a := range run {
		if strings.HasPrefix(a, "type=bind,src=") && strings.HasSuffix(a, ",dst="+DefaultWorkdir) {
			foundMount = true
		}
	}
	if !foundMount {
		t.Errorf("workspace bind mount (--mount type=bind,...,dst=%s) not found: %v", DefaultWorkdir, run)
	}
	// The keep-alive command makes it a long-lived container (D3).
	if run[len(run)-3] != DefaultImage || run[len(run)-2] != "sleep" || run[len(run)-1] != "infinity" {
		t.Errorf("expected `<image> sleep infinity` tail: %v", run[len(run)-3:])
	}
	// No runtime flag by default => IsolationProcess.
	if has(run, "--runtime=runsc") {
		t.Error("default opts must not request gVisor")
	}
}

func TestRuntimeRunscFlagAndCapabilities(t *testing.T) {
	sb, fr := newFake(t, Options{Runtime: "runsc"})
	if !has(fr.calls[0], "--runtime=runsc") {
		t.Errorf("runsc runtime not threaded into run argv: %v", fr.calls[0])
	}
	if got := sb.Capabilities().Isolation; got != sandbox.IsolationKernel {
		t.Errorf("Capabilities.Isolation = %v, want kernel (gVisor)", got)
	}
}

func TestCapabilitiesDefaultIsProcess(t *testing.T) {
	sb, _ := newFake(t, Options{})
	caps := sb.Capabilities()
	if caps.Isolation != sandbox.IsolationProcess {
		t.Errorf("Isolation = %v, want process", caps.Isolation)
	}
	if caps.Network {
		t.Error("Network = true, want false (network off by default)")
	}
}

func TestNetworkOptIn(t *testing.T) {
	sb, fr := newFake(t, Options{Network: true})
	if !hasFlagPair(fr.calls[0], "--network", "bridge") {
		t.Errorf("Network:true should request --network bridge: %v", fr.calls[0])
	}
	if !sb.Capabilities().Network {
		t.Error("Capabilities.Network should be true when opted in")
	}
}

func TestExtraMountsAndEnv(t *testing.T) {
	sb, fr := newFake(t, Options{
		ExtraMounts: []string{"/home/u/go/pkg/mod:/go/pkg/mod:ro"},
		ExtraEnv:    []string{"GOPROXY=off"},
	})
	if !has(fr.calls[0], "/home/u/go/pkg/mod:/go/pkg/mod:ro") {
		t.Errorf("extra mount not present in run argv: %v", fr.calls[0])
	}
	// ExtraEnv lands on the exec, not the run.
	_, _ = sb.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "true"}})
	if !hasFlagPair(fr.lastCall(), "-e", "GOPROXY=off") {
		t.Errorf("ExtraEnv not threaded into exec: %v", fr.lastCall())
	}
}

func TestExecArgvMapping(t *testing.T) {
	sb, fr := newFake(t, Options{})
	_, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "echo hi"},
		Dir: "sub", Env: []string{"K=V"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	ex := fr.lastCall()
	if ex[0] != "exec" {
		t.Errorf("not a docker exec: %v", ex)
	}
	if !hasFlagPair(ex, "-w", DefaultWorkdir+"/sub") {
		t.Errorf("working dir not joined under workspace: %v", ex)
	}
	if !hasFlagPair(ex, "-e", "K=V") {
		t.Errorf("per-command env not threaded: %v", ex)
	}
	if !has(ex, "fake-container-id") {
		t.Errorf("container id not in exec argv: %v", ex)
	}
	// The command tail is preserved in order.
	if ex[len(ex)-3] != "sh" || ex[len(ex)-2] != "-c" || ex[len(ex)-1] != "echo hi" {
		t.Errorf("command tail wrong: %v", ex[len(ex)-3:])
	}
}

func TestExecStdinTriggersInteractive(t *testing.T) {
	sb, fr := newFake(t, Options{})
	_, _ = sb.Exec(context.Background(), sandbox.Command{
		Path: "cat", Stdin: []byte("piped"),
	})
	if !has(fr.lastCall(), "-i") {
		t.Errorf("stdin should add -i to exec: %v", fr.lastCall())
	}
	if string(fr.stdins[len(fr.stdins)-1]) != "piped" {
		t.Errorf("stdin not forwarded to runner")
	}
}

func TestExecTimeoutWrapsCommand(t *testing.T) {
	sb, fr := newFake(t, Options{})
	_, _ = sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "sleep 9"}, Timeout: 3 * time.Second,
	})
	ex := fr.lastCall()
	// The command must run under an in-container `timeout -s KILL <secs>` so a
	// runaway is reaped as a process group, not orphaned (§13).
	if !has(ex, "timeout") || !hasFlagPair(ex, "-s", "KILL") || !has(ex, "3") {
		t.Errorf("timeout wrapper not applied: %v", ex)
	}
}

func TestExecTimedOutResult(t *testing.T) {
	// `timeout` exits 137 when it SIGKILLs the command — that maps to TimedOut.
	sb, _ := newFake(t, Options{}, reply{stdout: "id"}, reply{exit: 137})
	res, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "sleep 9"}, Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("exit 137 under a timeout should set TimedOut: %+v", res)
	}
}

func TestExecNonZeroIsNotError(t *testing.T) {
	// Parity with local.Exec: a non-zero exit is a Result, never a Go error (P6).
	sb, _ := newFake(t, Options{}, reply{stdout: "id"}, reply{exit: 3, stderr: "boom"})
	res, err := sb.Exec(context.Background(), sandbox.Command{Path: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatalf("non-zero exit returned a Go error: %v", err)
	}
	if res.ExitCode != 3 || string(res.Stderr) != "boom" {
		t.Errorf("Result mapping wrong: %+v", res)
	}
	if res.TimedOut {
		t.Error("a plain non-zero exit must not be marked TimedOut")
	}
}

func TestCloseRemovesContainer(t *testing.T) {
	sb, fr := newFake(t, Options{})
	if err := sb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	last := fr.lastCall()
	if last[0] != "rm" || !has(last, "-f") || !has(last, "fake-container-id") {
		t.Errorf("Close should `rm -f <id>`: %v", last)
	}
	// Idempotent: a second Close issues no further docker calls.
	before := len(fr.calls)
	if err := sb.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(fr.calls) != before {
		t.Error("second Close should be a no-op")
	}
}

func TestCappedBufferDropsExcess(t *testing.T) {
	b := &cappedBuffer{max: 4}
	n, _ := b.Write([]byte("hello world")) // 11 bytes into a 4-byte cap.
	if n != 11 {
		t.Errorf("Write reported %d, want 11 (must claim the whole write so the process never blocks)", n)
	}
	if got := string(b.Bytes()); got != "hell" {
		t.Errorf("retained %q, want first 4 bytes \"hell\"", got)
	}
	// A zero/negative cap means unlimited.
	u := &cappedBuffer{max: 0}
	u.Write([]byte("everything"))
	if string(u.Bytes()) != "everything" {
		t.Errorf("unlimited buffer dropped data: %q", u.Bytes())
	}
}

func TestInvalidUserSpecRejected(t *testing.T) {
	fr := &fakeRunner{}
	if _, err := newWithRunner(t.TempDir(), Options{User: "notauser"}, fr); err == nil {
		t.Error("a malformed User spec should be rejected before touching docker")
	}
}

func TestInvalidWorkdirMountRejected(t *testing.T) {
	for _, w := range []string{"/", "relative/path", "/has:colon", "/trailing/.."} {
		fr := &fakeRunner{}
		if _, err := newWithRunner(t.TempDir(), Options{WorkdirMount: w}, fr); err == nil {
			t.Errorf("WorkdirMount %q should be rejected", w)
		}
	}
}

func TestExecRefusesDirEscape(t *testing.T) {
	sb, _ := newFake(t, Options{})
	if _, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "pwd"}, Dir: "../../etc",
	}); err == nil {
		t.Error("an escaping cmd.Dir must be refused (fence parity with local)")
	}
}

func TestExecRefusesEmptyPath(t *testing.T) {
	sb, _ := newFake(t, Options{})
	if _, err := sb.Exec(context.Background(), sandbox.Command{Path: ""}); err == nil {
		t.Error("empty cmd.Path must be refused")
	}
}

func TestTimeoutRoundsUp(t *testing.T) {
	sb, fr := newFake(t, Options{})
	// 1500ms must round UP to 2s, never floor to 1s (which would kill early).
	_, _ = sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "true"}, Timeout: 1500 * time.Millisecond,
	})
	if !has(fr.lastCall(), "2") || !has(fr.lastCall(), "timeout") {
		t.Errorf("1500ms timeout should round up to `timeout ... 2`: %v", fr.lastCall())
	}
}
