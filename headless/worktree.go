package headless

import (
	"context"

	"github.com/AccursedGalaxy/driver-os/vcs"
)

func worktreeAdd(origCwd string) (vcs.WorktreeInfo, error) {
	return vcs.WorktreeAdd(context.Background(), origCwd)
}

func worktreeCollect(dir, patchPath string) (bool, error) {
	return vcs.WorktreeCollect(context.Background(), dir, patchPath)
}

func worktreeRemove(dir string) error {
	return vcs.WorktreeRemove(context.Background(), dir)
}
