//go:build docker_integration

// These tests need a REAL docker daemon and the built sandbox image. They are
// gated behind the docker_integration build tag so the default `go test ./...`
// (and CI without a daemon) never tries to run them. Run them with:
//
//	make sandbox-image          # once, builds driver-os-sandbox:latest
//	make sandbox-integration    # go test -tags docker_integration ./sandbox/docker
//
// If the daemon or image is missing, every test SKIPS with a clear message —
// never a silent pass (the plan's §9 rule: print what was skipped).
package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/sandboxtest"
)

// requireDocker skips the test (loudly) unless a daemon is reachable and the
// sandbox image is present. A skip here means "couldn't verify", which must be
// visible — not mistaken for a pass.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("SKIP: docker CLI not found — run `make sandbox-image` on a host with docker")
	}
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Skipf("SKIP: docker daemon not reachable (%v): %s", err, out)
	}
	if err := exec.Command("docker", "image", "inspect", DefaultImage).Run(); err != nil {
		t.Skipf("SKIP: image %s not built — run `make sandbox-image`", DefaultImage)
	}
}

func newReal(t *testing.T, dir string, opts Options) *Sandbox {
	t.Helper()
	sb, err := New(context.Background(), dir, opts)
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// TestConformanceDocker holds the docker backend to the SAME behavioral contract
// as local — the parity guarantee (Slice 3). If this passes, the two backends are
// interchangeable behind the interface.
func TestConformanceDocker(t *testing.T) {
	requireDocker(t)
	sandboxtest.RunConformance(t, func(t *testing.T, dir string) (sandbox.Sandbox, func()) {
		sb, err := New(context.Background(), dir, Options{})
		if err != nil {
			t.Fatalf("docker.New: %v", err)
		}
		return sb, func() { _ = sb.Close() }
	})
}

// TestSessionConformanceDocker holds the docker Session to the SAME stateful
// contract as the local Session — cwd/env persist across `docker exec` calls,
// sessions are isolated, etc. This is the real proof the shell-wrapper survives a
// genuinely stateless `docker exec` (busybox ash in the container), not just the
// host shell.
func TestSessionConformanceDocker(t *testing.T) {
	requireDocker(t)
	sandboxtest.RunSessionConformance(t, func(t *testing.T, dir string) (sandbox.Sessioner, func()) {
		sb, err := New(context.Background(), dir, Options{})
		if err != nil {
			t.Fatalf("docker.New: %v", err)
		}
		return sb, func() { _ = sb.Close() }
	})
}

// TestNetworkNoneBlocksEgress proves --network none (the default) actually stops
// outbound traffic — the core containment property for untrusted code (criterion
// #3). With no interface but loopback, any connect to a routable IP fails.
func TestNetworkNoneBlocksEgress(t *testing.T) {
	requireDocker(t)
	sb := newReal(t, t.TempDir(), Options{}) // Network defaults to off.
	res, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh",
		// 1.1.1.1 is routable on a normal network; with --network none there is no
		// route, so wget fails fast. -T bounds it so the test can't hang.
		Args:    []string{"-c", "wget -T 3 -q -O /dev/null http://1.1.1.1/ && echo REACHED || echo BLOCKED"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(string(res.Stdout), "REACHED") {
		t.Errorf("network egress was NOT blocked: stdout=%q", res.Stdout)
	}
}

// TestHostFilesUnreadable proves a host file OUTSIDE the workspace cannot be read
// from inside the container — only /workspace is mounted, so nothing else of the
// host's is visible (criterion #3).
func TestHostFilesUnreadable(t *testing.T) {
	requireDocker(t)
	// A secret in a sibling dir that is NOT bind-mounted.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("HOST-ONLY-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb := newReal(t, t.TempDir(), Options{})
	// Try the absolute host path AND a traversal out of /workspace; neither exists
	// in the container's mount namespace.
	res, _ := sb.Exec(context.Background(), sandbox.Command{
		Path:    "sh",
		Args:    []string{"-c", "cat " + secret + " 2>/dev/null; cat /workspace/../secret.txt 2>/dev/null; true"},
		Timeout: 10 * time.Second,
	})
	if strings.Contains(string(res.Stdout), "HOST-ONLY-SECRET") {
		t.Errorf("a host file outside the workspace was readable in-container: %q", res.Stdout)
	}
}

// TestImageHasToolset proves the trusted base carries everything the agent's
// tools shell out to (§7). A missing tool would silently break `run`/`search`.
func TestImageHasToolset(t *testing.T) {
	requireDocker(t)
	sb := newReal(t, t.TempDir(), Options{})
	for _, tool := range []string{"sh", "rg", "git", "go", "timeout"} {
		res, err := sb.Exec(context.Background(), sandbox.Command{
			Path: "sh", Args: []string{"-c", "command -v " + tool}, Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("Exec(command -v %s): %v", tool, err)
		}
		if res.ExitCode != 0 {
			t.Errorf("image is missing %q (exit %d) — a tool will break", tool, res.ExitCode)
		}
	}
}

// TestMemoryLimitBites proves --memory contains a memory bomb: a process trying
// to allocate far past the cap is OOM-killed, not allowed to exhaust host RAM.
func TestMemoryLimitBites(t *testing.T) {
	requireDocker(t)
	sb := newReal(t, t.TempDir(), Options{Memory: "32m"})
	// Allocate ~256MB into a shell variable — well past the 32m cgroup cap, so the
	// kernel OOM-kills it. A successful completion would mean the cap didn't bite.
	res, err := sb.Exec(context.Background(), sandbox.Command{
		Path:    "sh",
		Args:    []string{"-c", "x=$(head -c 268435456 /dev/zero | tr '\\0' 'a'); echo SURVIVED"},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(string(res.Stdout), "SURVIVED") {
		t.Errorf("memory limit did NOT bite — a 256MB alloc survived a 32m cap")
	}
}

// TestPidsLimitBites proves --pids-limit contains a fork bomb: past the cap, new
// processes can't be created. We use a deliberately tiny cap and a short timeout
// so the test is bounded and the container is torn down regardless.
func TestPidsLimitBites(t *testing.T) {
	requireDocker(t)
	sb := newReal(t, t.TempDir(), Options{PidsLimit: 16})
	res, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh",
		// Spawn background sleeps until fork fails; if we ever print ALL_FORKED the
		// cap didn't bite. timeout backstops the whole thing.
		Args:    []string{"-c", "i=0; while [ $i -lt 200 ]; do sleep 5 & i=$((i+1)); done; echo ALL_FORKED"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(string(res.Stdout), "ALL_FORKED") {
		t.Errorf("pids limit did NOT bite — 200 forks succeeded under a 16-pid cap")
	}
}

// TestTimeoutKillsInContainerProcess proves a timeout actually kills the
// in-container process tree (the orphan gotcha, §13): after a `sleep` is killed
// by its Timeout, no sleep process is left running in the container.
func TestTimeoutKillsInContainerProcess(t *testing.T) {
	requireDocker(t)
	sb := newReal(t, t.TempDir(), Options{})
	res, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "sleep 30"}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut=false; the 2s budget should have killed `sleep 30`")
	}
	// Confirm nothing is left: ask the container for lingering sleep processes.
	check, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "ps -o args= 2>/dev/null | grep -c '[s]leep 30' || true"}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec(ps): %v", err)
	}
	if n := strings.TrimSpace(string(check.Stdout)); n != "" && n != "0" {
		t.Errorf("orphaned process left after timeout: %q sleep processes still running", n)
	}
}

// TestReadOnlyModuleCacheMount proves the network-off module story: a read-only
// host GOMODCACHE mount lets an in-container build use cached modules while
// --network none stays on, AND the mount cannot be written by hostile code.
func TestReadOnlyModuleCacheMount(t *testing.T) {
	requireDocker(t)
	gomodcache := strings.TrimSpace(runHost(t, "go", "env", "GOMODCACHE"))
	if gomodcache == "" {
		t.Skip("SKIP: no host GOMODCACHE to mount")
	}
	sb := newReal(t, t.TempDir(), Options{
		ExtraMounts: []string{gomodcache + ":/go/pkg/mod:ro"},
	})
	// The mount must be READ-ONLY: a write attempt fails (can't poison the host cache).
	res, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "touch /go/pkg/mod/EVIL 2>&1 && echo WROTE || echo READONLY"}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(string(res.Stdout), "WROTE") {
		t.Errorf("module cache mount was writable — hostile code could poison the host cache: %q", res.Stdout)
	}

	// A stdlib-only build must succeed with the network off and GOPROXY=off (it
	// needs no modules at all) — proving the toolchain works under containment.
	if err := sb.WriteFile(context.Background(), "main.go",
		[]byte("package main\nimport \"fmt\"\nfunc main(){ fmt.Println(\"ok\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(context.Background(), "go.mod", []byte("module ex\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build, err := sb.Exec(context.Background(), sandbox.Command{
		Path: "sh", Args: []string{"-c", "go build -o /tmp/ex . && echo BUILT"}, Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec(go build): %v", err)
	}
	if !strings.Contains(string(build.Stdout), "BUILT") {
		t.Errorf("stdlib build failed under --network none: exit=%d stdout=%q stderr=%q", build.ExitCode, build.Stdout, build.Stderr)
	}
}

// TestContainerReapedOnClose proves Close removes the container — no leaks.
func TestContainerReapedOnClose(t *testing.T) {
	requireDocker(t)
	sb, err := New(context.Background(), t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := sb.id
	if err := sb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if exec.Command("docker", "inspect", id).Run() == nil {
		t.Errorf("container %s still exists after Close — leaked", id)
	}
}

func runHost(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out)
}
