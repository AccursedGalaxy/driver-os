package agent

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

func TestStandingBlockGoldenAndStaleness(t *testing.T) {
	_, root := gitWorkspace(t, map[string]string{"a.txt": "old\n"})
	ctx := context.Background()
	cfg := Config{Root: root, StandingContext: true, VerifyCmd: "go test ./...", SkipVerifyBaseline: true, Obs: nopObserver{}}
	gs := mustNewGates(t, ctx, cfg, 0)
	gs.verifyBaselineMeasured = true
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ss := newStandingState()
	defer ss.cleanup()
	tr := newTurnTracker(cfg, 8)
	cur, err := ss.currentTree(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr.observeRun("exit 0 (1ms)\nstdout:\nok")
	tr.recordRun("go test ./...", "exit 0 (1ms)\nstdout:\nok", cur)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("newer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ss.block(ctx, cfg, gs, tr, 1)
	want := "=== STANDING CONTEXT (auto-refreshed each turn — do NOT run `git diff` or re-read\n    files just to reconstruct this summary; read files when you need details not\n    shown here) ===\n\n# Your changes so far (vs session start)\n a.txt | 2 +-\n 1 file changed, 1 insertion(+), 1 deletion(-)\ndiff --git a/a.txt b/a.txt\nindex 3367afd..d58ed19 100644\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-old\n+newer\n\n# Last verification\nbaseline at session start: GREEN\ngate: `go test ./...` → PASS  [STALE: files changed since this ran — re-run to confirm]\nexit 0 (1ms)\nstdout:\nok\n=== END STANDING CONTEXT ==="
	if got != want {
		t.Fatalf("standing block mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestStandingDiffRenderSpy(t *testing.T) {
	_, root := gitWorkspace(t, map[string]string{"a.txt": "old\n"})
	ctx := context.Background()
	cfg := Config{Root: root, StandingContext: true, Obs: nopObserver{}}
	gs := mustNewGates(t, ctx, cfg, 0)
	ss := newStandingState()
	defer ss.cleanup()
	defer func() { standingDiffTrees = vcs.DiffTrees }()
	calls := 0
	standingDiffTrees = func(ctx context.Context, dir, base, cur string) (string, error) {
		calls++
		return vcs.DiffTrees(ctx, dir, base, cur)
	}
	tr := newTurnTracker(cfg, 8)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ss.block(ctx, cfg, gs, tr, 1)
	if calls != 1 {
		t.Fatalf("first block diff renders = %d, want 1", calls)
	}
	ss.block(ctx, cfg, gs, tr, 2)
	if calls != 1 {
		t.Fatalf("unchanged block diff renders = %d, want 1", calls)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("newer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ss.block(ctx, cfg, gs, tr, 3)
	if calls != 2 {
		t.Fatalf("changed block diff renders = %d, want 2", calls)
	}
}

func TestStandingVerificationSkipsRunNudgeWhenUnchanged(t *testing.T) {
	got := renderVerification(newTurnTracker(Config{VerifyCmd: "go test ./..."}, 8), "go test ./...", "tree1", false, true)
	want := "gate: `go test ./...` — no file changes yet this session; nothing to verify (the harness runs it authoritatively at finish)"
	if got != want {
		t.Fatalf("unchanged verification = %q, want %q", got, want)
	}
}

func TestStandingVerificationStillNudgesWhenChanged(t *testing.T) {
	got := renderVerification(newTurnTracker(Config{VerifyCmd: "go test ./..."}, 8), "go test ./...", "tree2", false, false)
	want := "gate: `go test ./...` — NOT run yet this session; the closing gate runs authoritatively at finish. For mid-run confidence, run only tests scoped to the package(s) you changed."
	if got != want {
		t.Fatalf("changed verification = %q, want %q", got, want)
	}
	if strings.Contains(got, "run it to confirm") {
		t.Fatalf("changed verification must not nudge the solver to run the full gate: %s", got)
	}
}

func TestStandingVerificationWithSkipCarriesFilter(t *testing.T) {
	const verifyCmd = "go test ./agent/ -skip 'TestA|TestB'"
	got := renderVerification(newTurnTracker(Config{VerifyCmd: verifyCmd}, 8), verifyCmd, "tree2", false, false)
	want := "gate: `go test ./agent/ -skip 'TestA|TestB'` — NOT run yet this session; the closing gate runs authoritatively at finish. For mid-run confidence, run only tests scoped to the package(s) you changed. The gate deliberately skips these — carry the filter or you will see unrelated red tests."
	if got != want {
		t.Fatalf("changed verification = %q, want %q", got, want)
	}
}

func TestStandingDiffCapShowsStatOnlyNoMalformedHunk(t *testing.T) {
	big := strings.Repeat("x\n", standingDiffCap+100)
	got := renderDiffSection(" big.txt | 7000 +++++\n 1 file changed, 7000 insertions(+)", "@@ -0,0 +1,7000 @@\n+"+big)
	if !strings.Contains(got, "over the 6000 cap; showing summary only") {
		t.Fatalf("missing over-cap notice:\n%s", got)
	}
	if strings.Contains(got, "@@ -0,0") {
		t.Fatalf("over-cap diff included a hunk:\n%s", got)
	}
}

func TestStandingDirtyBaselineScopesToModelChanges(t *testing.T) {
	_, root := gitWorkspace(t, map[string]string{"a.txt": "pre\n", "b.txt": "old\n"})
	ctx := context.Background()
	// User dirt already present before standing baseline.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("user dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Root: root, StandingContext: true, Obs: nopObserver{}}
	gs := mustNewGates(t, ctx, cfg, 0)
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("model change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ss := newStandingState()
	defer ss.cleanup()
	got := ss.block(ctx, cfg, gs, newTurnTracker(cfg, 8), 1)
	if strings.Contains(got, "a.txt") || !strings.Contains(got, "b.txt") {
		t.Fatalf("diff should show only model-edited b.txt, not pre-existing a.txt:\n%s", got)
	}
}

func TestStandingGatePersistenceNonGateDoesNotClobber(t *testing.T) {
	cfg := Config{VerifyCmd: "go test ./..."}
	tr := newTurnTracker(cfg, 8)
	tr.observeRun("exit 0 (1ms)\nstdout:\ngreen")
	tr.recordRun("go test ./...", "exit 0 (1ms)\nstdout:\ngreen", "tree1")
	tr.observeRun("exit 0 (1ms)\nstdout:\nlisting")
	tr.recordRun("ls", "exit 0 (1ms)\nstdout:\nlisting", "tree1")
	got := renderVerification(tr, cfg.VerifyCmd, "tree1", false, false)
	if !strings.Contains(got, "gate: `go test ./...` → PASS") || !strings.Contains(got, "green") {
		t.Fatalf("gate pass/tail clobbered by non-gate run:\n%s", got)
	}
	if !strings.Contains(got, "last check (not the gate): `ls` → PASS") || strings.Contains(got, "listing\nlisting") {
		t.Fatalf("non-gate last check not rendered compactly:\n%s", got)
	}
}

func TestStandingTypedVerificationStatusesAndFreshReplacement(t *testing.T) {
	cfg := Config{VerifyCmd: "go test ./..."}
	tr := newTurnTracker(cfg, 8)

	tr.recordRun(cfg.VerifyCmd, "exit 1 (2ms) [timed out — narrow the command's work or raise its own limits]", "old")
	if got := renderVerification(tr, cfg.VerifyCmd, "current", false, false); !strings.Contains(got, "→ TIMEOUT  [STALE:") {
		t.Fatalf("typed timeout/staleness not rendered:\n%s", got)
	}
	tr.recordRun(cfg.VerifyCmd, "exit 1 (2ms)\nstderr:\ndisk quota exceeded", "current")
	if got := renderVerification(tr, cfg.VerifyCmd, "current", false, false); !strings.Contains(got, "→ ENV-FAULT") || strings.Contains(got, "[STALE:") {
		t.Fatalf("fresh infra result was laundered or remained stale:\n%s", got)
	}
	if tr.lastVerify.Status != EvidenceInconclusive || tr.lastVerify.Cause != VerificationCauseEnvironment || tr.lastVerify.Attempts != 2 {
		t.Fatalf("typed latest record = %+v", tr.lastVerify)
	}
}

func TestStandingFreshnessUnknown(t *testing.T) {
	cfg := Config{VerifyCmd: "go test ./..."}
	tr := newTurnTracker(cfg, 8)
	tr.observeRun("exit 0 (1ms)")
	tr.recordRun("go test ./...", "exit 0 (1ms)", "tree1")
	got := renderVerification(tr, cfg.VerifyCmd, "", true, false)
	if !strings.Contains(got, "[FRESHNESS UNKNOWN: could not read working tree]") {
		t.Fatalf("missing freshness unknown:\n%s", got)
	}
}

func TestWriteTreeCachedWarmIndex(t *testing.T) {
	_, root := gitWorkspace(t, map[string]string{"a.txt": "one\n"})
	ctx := context.Background()
	idx := filepath.Join(t.TempDir(), "index")
	base, err := vcs.WriteTreeCached(ctx, root, idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cur, err := vcs.WriteTreeCached(ctx, root, idx)
	if err != nil {
		t.Fatal(err)
	}
	if base == cur {
		t.Fatal("cached tree did not change after file mutation")
	}
}

type standingCaptureModel struct {
	calls []llm.Request
}

func (m *standingCaptureModel) Name() string { return "standing-capture" }
func (m *standingCaptureModel) Capabilities() llm.Capabilities {
	return llm.Capabilities{Tools: true}
}
func (m *standingCaptureModel) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("standing-capture")
}
func (m *standingCaptureModel) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	m.calls = append(m.calls, req)
	if len(m.calls) == 1 {
		return &llm.Response{Content: []llm.ContentPart{llm.ToolCallPart{ID: "n1", Name: "noop", Args: json.RawMessage(`{}`)}}}, nil
	}
	return &llm.Response{Content: []llm.ContentPart{llm.Text("done")}}, nil
}

func TestStandingContextDoesNotEnterPersistentMessages(t *testing.T) {
	sb := sbWith(t, nil)
	run := func(enabled bool) []llm.Request {
		m := &standingCaptureModel{}
		_, err := RunNative(context.Background(), Config{
			Model:           m,
			Sandbox:         sb,
			Task:            "do it",
			StandingContext: enabled,
			Tools: map[string]Tool{"noop": {
				Name:   "noop",
				Schema: json.RawMessage(`{"type":"object","properties":{}}`),
				RunJSON: func(context.Context, json.RawMessage) (string, error) {
					return "observed", nil
				},
			}},
			MaxIterations: 3,
			Obs:           nopObserver{},
		})
		if err != nil {
			t.Fatalf("RunNative(%v): %v", enabled, err)
		}
		return m.calls
	}
	off := run(false)
	on := run(true)
	if len(off) != len(on) || len(on) < 2 {
		t.Fatalf("call counts off=%d on=%d", len(off), len(on))
	}
	for i := range off {
		if !reflect.DeepEqual(off[i].Messages, on[i].Messages) {
			t.Fatalf("persistent Messages differ at call %d\noff=%#v\non=%#v", i, off[i].Messages, on[i].Messages)
		}
		if off[i].StandingContext != "" {
			t.Fatalf("feature-off request unexpectedly has standing context: %q", off[i].StandingContext)
		}
		if on[i].StandingContext == "" {
			t.Fatalf("feature-on request %d lacks standing context", i)
		}
	}
}
