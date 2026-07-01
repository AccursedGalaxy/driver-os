package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

const (
	// envMaxEntries caps the top-level listing in the ENVIRONMENT preamble so a
	// huge workspace root can't bloat the opening message (P1).
	envMaxEntries = 40
	// envObserveTimeout bounds the whole pre-turn observation: a slow backend
	// must not stall the run before the first model call.
	envObserveTimeout = 5 * time.Second
)

// observeEnvironment gathers the opening ENVIRONMENT preamble through the
// sandbox, BEFORE the first model turn: the working directory `run` commands
// start in (the WorkdirReporter capability) and the root's top-level entries
// (ListDir — no subprocess). Seeding these into the opening message kills two
// failure modes observed live: the near-universal "turn 1 = list_dir ." tax
// (paid by 3/3 runs in the 2026-07-01 hard-task probe), and the model guessing
// an absolute cwd (`cd /home/user && go test ./...` — two wasted turns).
//
// Everything is BEST-EFFORT: a part that can't be observed is omitted, and a
// nil sandbox — or one exposing neither piece — yields "" so the seed message
// is exactly the historical "TASK: …". This preamble is harness-observed, not
// model-observed: it does NOT count toward the grounded gate (P4), which stays
// about what the MODEL verified with tools this run.
func observeEnvironment(ctx context.Context, sb sandbox.Sandbox) string {
	if sb == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, envObserveTimeout)
	defer cancel()

	var b strings.Builder
	if wr, ok := sb.(sandbox.WorkdirReporter); ok {
		if wd := wr.Workdir(); wd != "" {
			fmt.Fprintf(&b, "\n- working directory: %s — `run` commands START here, and file-tool paths are relative to it; do NOT cd elsewhere to find the project", wd)
		}
	}
	if entries, err := sb.ListDir(ctx, "."); err == nil && len(entries) > 0 {
		shown, more := entries, 0
		if len(entries) > envMaxEntries {
			shown, more = entries[:envMaxEntries], len(entries)-envMaxEntries
		}
		names := make([]string, 0, len(shown))
		for _, e := range shown {
			kind := "file"
			if e.IsDir {
				kind = "dir"
			}
			names = append(names, kind+" "+e.Name)
		}
		line := strings.Join(names, ", ")
		if more > 0 {
			line += fmt.Sprintf(", … (+%d more — list_dir \".\" for the rest)", more)
		}
		fmt.Fprintf(&b, "\n- top-level entries: %s", line)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nENVIRONMENT (observed at start):" + b.String()
}
