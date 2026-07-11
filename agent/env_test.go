package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// The ENVIRONMENT preamble exists to kill two live-observed failure modes: the
// universal "turn 1 = list_dir ." exploration tax, and the model guessing an
// absolute cwd (`cd /home/user && …`). These tests pin its contract: grounded
// in real sandbox observations, best-effort, and seeded only on a FRESH run.

func TestObserveEnvironmentLocalSandbox(t *testing.T) {
	sb := sbWith(t, map[string]string{"go.mod": "module x\n", "cmd/main.go": "package main\n"})
	env := observeEnvironment(context.Background(), sb, false)
	for _, want := range []string{
		"ENVIRONMENT (observed at start):",
		"working directory: /", // the REAL absolute path, not a placeholder
		"dir cmd",
		"file go.mod",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("preamble missing %q; got:\n%s", want, env)
		}
	}
}

func TestObserveEnvironmentBestEffort(t *testing.T) {
	if got := observeEnvironment(context.Background(), nil, false); got != "" {
		t.Errorf("nil sandbox must yield an empty preamble, got %q", got)
	}
	// plainSandbox exposes neither WorkdirReporter nor a non-empty ListDir: the
	// preamble must vanish entirely (seed stays the historical "TASK: …").
	if got := observeEnvironment(context.Background(), &plainSandbox{}, false); got != "" {
		t.Errorf("bare sandbox must yield an empty preamble, got %q", got)
	}
}

// manyEntriesSandbox drives the listing cap.
type manyEntriesSandbox struct{ plainSandbox }

func (m *manyEntriesSandbox) ListDir(context.Context, string) ([]sandbox.DirEntry, error) {
	out := make([]sandbox.DirEntry, envMaxEntries+7)
	for i := range out {
		out[i] = sandbox.DirEntry{Name: strings.Repeat("f", 3) + string(rune('a'+i%26))}
	}
	return out, nil
}

func TestObserveEnvironmentCapsListing(t *testing.T) {
	env := observeEnvironment(context.Background(), &manyEntriesSandbox{}, false)
	if !strings.Contains(env, "+7 more") {
		t.Errorf("over-cap listing must be clipped with a count; got:\n%s", env)
	}
}

func TestSeedMessagesEnvOnlyWhenFresh(t *testing.T) {
	env := "\n\nENVIRONMENT (observed at start):\n- working directory: /w"
	fresh := seedMessagesT(Config{Task: "t"}, env)
	if len(fresh) != 1 || !strings.HasPrefix(fresh[0].Text(), "TASK: t") {
		t.Fatalf("fresh seed must keep the TASK framing first; got %v", fresh)
	}
	if !strings.Contains(fresh[0].Text(), "ENVIRONMENT") {
		t.Error("fresh seed must carry the preamble")
	}
	// A continuing conversation already holds its grounding — no re-seeding.
	cont := seedMessagesT(Config{Task: "next", History: []llm.Message{llm.User("TASK: earlier"), llm.Assistant("ok")}}, env)
	if got := cont[len(cont)-1].Text(); strings.Contains(got, "ENVIRONMENT") {
		t.Errorf("continuation turn must NOT repeat the preamble; got %q", got)
	}
}

// End-to-end through the native loop: the very first request the model sees
// opens with TASK + the observed environment.
func TestRunNativeSeedsEnvironmentPreamble(t *testing.T) {
	res, ns, _ := runNative(t,
		map[string]string{"go.mod": "module x\n"},
		[][]llm.ContentPart{{llm.Text("done")}},
	)
	if res.Outcome != Answered {
		t.Fatalf("outcome = %q, want Answered", res.Outcome)
	}
	first := ns.calls[0].Messages[0].Text()
	if !strings.HasPrefix(first, "TASK: test task") {
		t.Fatalf("first message must open with the TASK framing; got %q", first)
	}
	for _, want := range []string{"ENVIRONMENT", "working directory: /", "file go.mod"} {
		if !strings.Contains(first, want) {
			t.Errorf("first message missing %q; got:\n%s", want, first)
		}
	}
}

func TestFormatRunCdFailureHint(t *testing.T) {
	obs := formatRun(&sandbox.Result{
		ExitCode: 1,
		Stderr:   []byte("sh: line 1: cd: /home/user: No such file or directory"),
		Duration: 2 * time.Millisecond,
	})
	if !strings.Contains(obs, "START in the project root") {
		t.Errorf("failed cd must carry the recovery hint (P3); got:\n%s", obs)
	}
	// A clean run — or an unrelated failure — must stay hint-free.
	for _, r := range []*sandbox.Result{
		{ExitCode: 0, Stdout: []byte("ok")},
		{ExitCode: 1, Stderr: []byte("go: build failed")},
	} {
		if obs := formatRun(r); strings.Contains(obs, "project root") {
			t.Errorf("hint fired without a failed cd; got:\n%s", obs)
		}
	}
}
