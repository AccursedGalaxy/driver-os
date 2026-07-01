package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/session"
)

// The local backend is a Sessioner too — nearly free, since "session state" is
// just a state file in the host temp dir that successive Exec subprocesses read
// and rewrite (see ../../docs/specs/SESSION.md). This keeps the two backends behaviorally
// identical under the shared session conformance suite.
var _ sandbox.Sessioner = (*Sandbox)(nil)

var localSessionCounter atomic.Uint64

// NewSession opens a stateful session whose shell state persists in a per-session
// dir under the host temp dir (unique via pid + counter). The state lives OUTSIDE
// the workspace fence on purpose — it is our bookkeeping, not workspace content,
// and a local Exec subprocess writes there directly (the fence governs ReadFile/
// WriteFile, not what a spawned process touches). Close removes the dir.
func (s *Sandbox) NewSession(_ context.Context) (sandbox.Session, error) {
	n := localSessionCounter.Add(1)
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("driver-session-%d-%d", os.Getpid(), n))
	return session.New(s, dir), nil
}
