package docker

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/session"
)

// The docker backend is a Sessioner: it can open a stateful Session whose Exec
// calls share cwd + env (the foundation for the long-lived-process host the gopls
// tenant will need — see ../../SESSION.md).
var _ sandbox.Sessioner = (*Sandbox)(nil)

var dockerSessionCounter atomic.Uint64

// NewSession opens a stateful session over THIS already-running container — no new
// container is started; successive Exec calls just share shell state via a state
// file under the container's /tmp tmpfs (writable and container-lifetime). The
// state dir is unique per session (pid + counter) so independent sessions over the
// same container don't collide. The session's Close removes that dir; the
// container itself is reaped by the Sandbox's own Close.
func (s *Sandbox) NewSession(_ context.Context) (sandbox.Session, error) {
	n := dockerSessionCounter.Add(1)
	dir := "/tmp/driver-session-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(n, 10)
	return session.New(s, dir), nil
}
