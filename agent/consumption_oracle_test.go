package agent

// S6a SHIP GATE — the behavioral consumption oracle (PROFILES.md §7.4 #3,
// profile-FieldID scope): for every field an execution profile can declare,
// vary it to a distinguishable non-default value through runspec.Resolve and
// assert the value is OBSERVED AT ITS ACTUAL CONSUMPTION SITE, in both loops
// where the field has one. Structure alone cannot prove a resolved value
// reaches its consumer — a field can serialize exactly once and still be
// ignored or shadowed (the headless AutoVerify drop) — so every assertion here
// is on loop BEHAVIOR (the request the model received, the observation fed
// back, the detector that fired, the outcome), never on a startup snapshot.
//
// The table doubles as the field → consuming-component classification (§7.4
// #1's "named consumption site"): a NEW profile field fails
// TestProfileFieldConsumptionOracle until it is classified here, and a field
// classified builder-consumed documents why no loop assertion exists.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/profile"
	"github.com/AccursedGalaxy/driver-os/runspec"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

type consumptionCase struct {
	site   string // the consuming component the assertion observes.
	text   func(t *testing.T)
	native func(t *testing.T)
	// builderOnly documents fields resolved by runspec but consumed by the
	// BINARIES (workspace/memory construction), not the loops; the assertion
	// pins resolution-side threading, the loop halves are deliberately nil.
	builderOnly string
}

// oracleWorkspace builds a real directory + local sandbox (Content.Root needs
// the host path, unlike sbWith).
func oracleWorkspace(t *testing.T, files map[string]string) (sandbox.Sandbox, string) {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	return sb, root
}

func oracleResolve(t *testing.T, mut func(*runspec.RequestedConfig)) runspec.ResolvedSpec {
	t.Helper()
	var req runspec.RequestedConfig
	mut(&req)
	spec, _, err := runspec.Resolve(req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return spec
}

// distinctReads builds n distinct text-loop read_file replies.
func distinctReads(n int) []string {
	replies := make([]string, n)
	for i := range replies {
		replies[i] = fmt.Sprintf("read_file f%d.txt", i)
	}
	return replies
}

// requestsText flattens every message of every recorded request.
func requestsText(calls []llm.Request) string {
	var b strings.Builder
	for _, req := range calls {
		b.WriteString(req.System)
		for _, m := range req.Messages {
			for _, p := range m.Parts {
				if tp, ok := p.(llm.TextPart); ok {
					b.WriteString(tp.Text)
				}
				if tr, ok := p.(llm.ToolResultPart); ok {
					b.WriteString(tr.Content)
				}
			}
		}
	}
	return b.String()
}

func observationsText(res *RunResult) string {
	var b strings.Builder
	for _, s := range res.Steps {
		b.WriteString(s.Observation)
		b.WriteString("\n")
	}
	return b.String()
}

var consumptionSites = map[profile.FieldID]consumptionCase{
	profile.MaxItersField: {
		site: "loop iteration cap (for i := 1; i <= maxIter — both loop headers)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.MaxIterations = rsInt(3) })
			sp := &scripted{replies: append(distinctReads(3), "read_file overflow.txt")}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil || res.Outcome != HitCap || res.Iterations != 3 {
				t.Fatalf("outcome=%v iters=%d err=%v, want hit_cap at exactly 3", res.Outcome, res.Iterations, err)
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.MaxIterations = rsInt(3) })
			ns := &nativeScript{turns: readTurns(4)}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil || res.Outcome != HitCap || res.Iterations != 3 {
				t.Fatalf("outcome=%v iters=%d err=%v, want hit_cap at exactly 3", res.Outcome, res.Iterations, err)
			}
		},
	},
	profile.MaxTokensField: {
		site: "llm.Request.MaxTokens on every model call (loop request build)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.MaxTokens = rsInt(1234) })
			sp := &scripted{replies: []string{"answer done"}}
			if _, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if len(sp.calls) == 0 || sp.calls[0].MaxTokens != 1234 {
				t.Fatalf("model saw MaxTokens %d, want 1234", sp.calls[0].MaxTokens)
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.MaxTokens = rsInt(1234) })
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			if _, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if len(ns.calls) == 0 || ns.calls[0].MaxTokens != 1234 {
				t.Fatalf("model saw MaxTokens %d, want 1234", ns.calls[0].MaxTokens)
			}
		},
	},
	profile.EffortField: {
		site: "llm.Request.ReasoningEffort on every model call (loop request build)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ReasoningEffort = rsString("xhigh") })
			sp := &scripted{replies: []string{"answer done"}}
			if _, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if sp.calls[0].ReasoningEffort != "xhigh" {
				t.Fatalf("model saw effort %q, want xhigh", sp.calls[0].ReasoningEffort)
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ReasoningEffort = rsString("xhigh") })
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			if _, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if ns.calls[0].ReasoningEffort != "xhigh" {
				t.Fatalf("model saw effort %q, want xhigh", ns.calls[0].ReasoningEffort)
			}
		},
	},
	profile.RunTimeoutField: {
		site: "DefaultTools `run` execution bound (wrapTools → runOp kill)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.RunTimeout = rsDuration(150 * time.Millisecond) })
			sp := &scripted{replies: []string{"run sleep 1", "answer done"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "[timed out") {
				t.Fatalf("a 1s sleep under a 150ms run timeout did not time out:\n%s", observationsText(res))
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.RunTimeout = rsDuration(150 * time.Millisecond) })
			ns := &nativeScript{turns: [][]llm.ContentPart{
				{structuredCall("c1", "run", map[string]any{"command": "sleep 1"})},
				{llm.Text("done")},
			}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "[timed out") {
				t.Fatalf("a 1s sleep under a 150ms run timeout did not time out:\n%s", observationsText(res))
			}
		},
	},
	profile.VerifyContinueField: {
		site: "gates.finish continue-on-fail (verifyState.VerifyContinue)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) {
				r.VerifyCmd = rsString("false")
				r.VerifyContinue = rsBool(true)
				r.SkipVerifyBaseline = rsBool(true)
				r.MaxIterations = rsInt(2)
			})
			sp := &scripted{replies: []string{"answer a1", "answer a2"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if len(sp.calls) != 2 || res.Iterations != 2 {
				t.Fatalf("calls=%d iters=%d — a rejected finish under VerifyContinue must feed back and continue", len(sp.calls), res.Iterations)
			}
			if !strings.Contains(requestsText(sp.calls), "Keep working") {
				t.Fatal("the continue-on-fail feedback never reached the model")
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) {
				r.VerifyCmd = rsString("false")
				r.VerifyContinue = rsBool(true)
				r.SkipVerifyBaseline = rsBool(true)
				r.MaxIterations = rsInt(2)
			})
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("a1")}, {llm.Text("a2")}}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if len(ns.calls) != 2 || res.Iterations != 2 {
				t.Fatalf("calls=%d iters=%d — a rejected finish under VerifyContinue must feed back and continue", len(ns.calls), res.Iterations)
			}
			if !strings.Contains(requestsText(ns.calls), "Keep working") {
				t.Fatal("the continue-on-fail feedback never reached the model")
			}
		},
	},
	profile.AutoVerifyField: {
		site: "resolveAutoVerify derivation attempt (verifyState mutation + RunResult auto-verify hand-off)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, map[string]string{"go.mod": "module oracle\n\ngo 1.22\n"})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.AutoVerify = rsBool(true) })
			sp := &scripted{replies: []string{"answer done"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !res.autoVerifyResolved {
				t.Fatal("AutoVerify=true never reached resolveAutoVerify (no derivation attempt recorded)")
			}
			off := oracleResolve(t, func(r *runspec.RequestedConfig) { r.AutoVerify = rsBool(false) })
			sp2 := &scripted{replies: []string{"answer done"}}
			res2, err := Run(context.Background(), off, Runtime{Model: sp2, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if res2.autoVerifyResolved {
				t.Fatal("AutoVerify=false still derived a gate")
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, map[string]string{"go.mod": "module oracle\n\ngo 1.22\n"})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.AutoVerify = rsBool(true) })
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !res.autoVerifyResolved {
				t.Fatal("AutoVerify=true never reached resolveAutoVerify (no derivation attempt recorded)")
			}
		},
	},
	profile.AutoVerifySoftField: {
		site: "verifyCompletionFailure soft-accept after the bounded feedback budget (verifyState.AutoVerifySoft)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) {
				r.VerifyCmd = rsString("false")
				r.AutoVerifySoft = rsBool(true)
				r.VerifyContinue = rsBool(true)
				r.SkipVerifyBaseline = rsBool(true)
				r.MaxIterations = rsInt(6)
			})
			sp := &scripted{replies: []string{"answer a1", "answer a2", "answer a3"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != Answered || res.Iterations != 3 {
				t.Fatalf("outcome=%v iters=%d — a SOFT red gate must become advisory after the feedback budget (accept on the 3rd finish)", res.Outcome, res.Iterations)
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) {
				r.VerifyCmd = rsString("false")
				r.AutoVerifySoft = rsBool(true)
				r.VerifyContinue = rsBool(true)
				r.SkipVerifyBaseline = rsBool(true)
				r.MaxIterations = rsInt(6)
			})
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("a1")}, {llm.Text("a2")}, {llm.Text("a3")}}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != Answered || res.Iterations != 3 {
				t.Fatalf("outcome=%v iters=%d — a SOFT red gate must become advisory after the feedback budget (accept on the 3rd finish)", res.Outcome, res.Iterations)
			}
		},
	},
	profile.MemoryField: {
		site:        "BUILDER-consumed: binaries construct mneme when the resolved Memory is true; the loop consumes the resulting Runtime.Memory binding, not the flag",
		builderOnly: "resolution threading pinned here; construction is per-binary",
	},
	profile.WorktreeField: {
		site:        "BUILDER-consumed: binaries create/require the worktree from the resolved Worktree posture before the loop starts",
		builderOnly: "resolution threading pinned here; construction is per-binary",
	},
	profile.BootContextField: {
		site: "observeEnvironment dependency digest in the opening ENVIRONMENT preamble",
		text: func(t *testing.T) {
			files := map[string]string{"go.mod": "module oracle\n\ngo 1.22\n"}
			sb, root := oracleWorkspace(t, files)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.BootContext = rsBool(true) })
			sp := &scripted{replies: []string{"answer done"}}
			if _, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(requestsText(sp.calls), "- dependencies (module oracle") {
				t.Fatal("BootContext=true did not add the dependency digest to the preamble")
			}
			off := oracleResolve(t, func(r *runspec.RequestedConfig) { r.BootContext = rsBool(false) })
			sp2 := &scripted{replies: []string{"answer done"}}
			if _, err := Run(context.Background(), off, Runtime{Model: sp2, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(requestsText(sp2.calls), "- dependencies (module oracle") {
				t.Fatal("BootContext=false still added the dependency digest")
			}
		},
		native: func(t *testing.T) {
			files := map[string]string{"go.mod": "module oracle\n\ngo 1.22\n"}
			sb, root := oracleWorkspace(t, files)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.BootContext = rsBool(true) })
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			if _, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(requestsText(ns.calls), "- dependencies (module oracle") {
				t.Fatal("BootContext=true did not add the dependency digest to the preamble")
			}
		},
	},
	profile.StandingContextField: {
		site: "standingState.block appended as a per-turn user message (native loop only; the text loop has no standing feed)",
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.StandingContext = rsBool(true) })
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			if _, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(requestsText(ns.calls), "=== STANDING CONTEXT") {
				t.Fatal("StandingContext=true never rendered the standing block")
			}
			off := oracleResolve(t, func(r *runspec.RequestedConfig) { r.StandingContext = rsBool(false) })
			ns2 := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			if _, err := RunNative(context.Background(), off, Runtime{Model: ns2, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(requestsText(ns2.calls), "=== STANDING CONTEXT") {
				t.Fatal("StandingContext=false still rendered the standing block")
			}
		},
	},
	profile.BatchReadsField: {
		site: "native system prompt batch-reads addendum (resolveSystemPrompt; the text loop's protocol prompt has no addenda)",
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.BatchReads = rsBool(true) })
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			if _, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(ns.calls[0].System, "BATCH INDEPENDENT READS") {
				t.Fatal("BatchReads=true never reached the system prompt")
			}
		},
	},
	profile.RequireDiffField: {
		site: "gates.requireDiffFailure empty-diff gate (pol.RequireDiff at gate construction)",
		text: func(t *testing.T) {
			sb, root := gitWorkspace(t, map[string]string{"a.txt": "x\n"})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.RequireDiff = rsBool(true) })
			sp := &scripted{replies: []string{"answer done"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != Unverified || !strings.Contains(res.Reason, "require-diff") {
				t.Fatalf("outcome=%v reason=%q — a no-change answer under RequireDiff must fail the completion gate", res.Outcome, res.Reason)
			}
		},
		native: func(t *testing.T) {
			sb, root := gitWorkspace(t, map[string]string{"a.txt": "x\n"})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.RequireDiff = rsBool(true) })
			ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != Unverified || !strings.Contains(res.Reason, "require-diff") {
				t.Fatalf("outcome=%v reason=%q — a no-change answer under RequireDiff must fail the completion gate", res.Outcome, res.Reason)
			}
		},
	},
	profile.ReadWindowField: {
		site: "DefaultTools read_file line window (wrapTools → ReadOptions.Window)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, map[string]string{"big.txt": strings.Repeat("line\n", 10)})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ReadWindow = rsInt(3) })
			sp := &scripted{replies: []string{"read_file big.txt", "answer done"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "[7 more line(s)") {
				t.Fatalf("a 10-line file under ReadWindow=3 was not clipped at 3:\n%s", observationsText(res))
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, map[string]string{"big.txt": strings.Repeat("line\n", 10)})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ReadWindow = rsInt(3) })
			ns := &nativeScript{turns: [][]llm.ContentPart{
				{structuredCall("c1", "read_file", map[string]any{"path": "big.txt"})},
				{llm.Text("done")},
			}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "[7 more line(s)") {
				t.Fatalf("a 10-line file under ReadWindow=3 was not clipped at 3:\n%s", observationsText(res))
			}
		},
	},
	profile.ReadOutlineField: {
		site: "DefaultTools read_file clipped-read outline (wrapTools → ReadOptions.Outline)",
		text: func(t *testing.T) {
			goFile := "package p\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n\nfunc D() {}\n"
			sb, root := oracleWorkspace(t, map[string]string{"big.go": goFile})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ReadWindow = rsInt(2); r.ReadOutline = rsBool(true) })
			sp := &scripted{replies: []string{"read_file big.go", "answer done"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "--- file outline") {
				t.Fatalf("ReadOutline=true never appended the outline to a clipped read:\n%s", observationsText(res))
			}
		},
		native: func(t *testing.T) {
			goFile := "package p\n\nfunc A() {}\n\nfunc B() {}\n\nfunc C() {}\n\nfunc D() {}\n"
			sb, root := oracleWorkspace(t, map[string]string{"big.go": goFile})
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ReadWindow = rsInt(2); r.ReadOutline = rsBool(true) })
			ns := &nativeScript{turns: [][]llm.ContentPart{
				{structuredCall("c1", "read_file", map[string]any{"path": "big.go"})},
				{llm.Text("done")},
			}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "--- file outline") {
				t.Fatalf("ReadOutline=true never appended the outline to a clipped read:\n%s", observationsText(res))
			}
		},
	},
	profile.ChurnNudgeRunsField: {
		site: "turnTracker.churnNudge threshold (trackerDeps.churnNudgeRuns)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ChurnNudgeRuns = rsInt(2) })
			sp := &scripted{replies: []string{"run false", "run false # again", "answer done"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "the tests have failed several times") {
				t.Fatalf("2 failing runs under ChurnNudgeRuns=2 never fired the churn nudge:\n%s", observationsText(res))
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.ChurnNudgeRuns = rsInt(2) })
			ns := &nativeScript{turns: [][]llm.ContentPart{
				{structuredCall("c1", "run", map[string]any{"command": "false"})},
				{structuredCall("c2", "run", map[string]any{"command": "false # again"})},
				{llm.Text("done")},
			}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "the tests have failed several times") {
				t.Fatalf("2 failing runs under ChurnNudgeRuns=2 never fired the churn nudge:\n%s", observationsText(res))
			}
		},
	},
	profile.NavSpiralWindowField: {
		site: "spiralState cycle window (TerminationPolicy.NavSpiralWindow → newSpiralState)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.NavSpiralWindow = rsInt(2); r.MaxIterations = rsInt(10) })
			sp := &scripted{replies: []string{"list_dir a", "list_dir b", "list_dir a", "list_dir b", "list_dir a"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != KilledSpiral || res.Iterations != 4 {
				t.Fatalf("outcome=%v iters=%d — 2 revisit turns under window=2 must kill at turn 4", res.Outcome, res.Iterations)
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.NavSpiralWindow = rsInt(2); r.MaxIterations = rsInt(10) })
			ns := &nativeScript{turns: [][]llm.ContentPart{
				{toolCall("c1", "list_dir", "a")},
				{toolCall("c2", "list_dir", "b")},
				{toolCall("c3", "list_dir", "a")},
				{toolCall("c4", "list_dir", "b")},
				{toolCall("c5", "list_dir", "a")},
			}}
			res, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != KilledSpiral || res.Iterations != 4 {
				t.Fatalf("outcome=%v iters=%d — 2 revisit turns under window=2 must kill at turn 4", res.Outcome, res.Iterations)
			}
		},
	},
	profile.AnswerNudgeWindowField: {
		site: "near-cap answer-forcer for observe-only toolsets (native loop; the text loop has no answer nudge)",
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.AnswerNudgeWindow = rsInt(2); r.MaxIterations = rsInt(4) })
			ns := &nativeScript{turns: readTurns(4)}
			if _, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb, Tools: observeOnlyToolsT(sb)}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(requestsText(ns.calls), "you are almost out of turns") {
				t.Fatal("AnswerNudgeWindow=2 never fired the answer-forcer for an observe-only toolset")
			}
		},
	},
	profile.FinishNudgeField: {
		site: "turnTracker.finishNudge near-cap finisher (trackerDeps.finishNudgeWindow)",
		text: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.FinishNudgeWindow = rsInt(2); r.MaxIterations = rsInt(4) })
			sp := &scripted{replies: []string{"run echo ok", "read_file x1.txt", "read_file x2.txt", "read_file x3.txt"}}
			res, err := Run(context.Background(), spec, Runtime{Model: sp, Sandbox: sb}, Content{Task: "t", Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(observationsText(res), "your last build/test run passed") {
				t.Fatalf("FinishNudgeWindow=2 with a green run and stable files never fired:\n%s", observationsText(res))
			}
		},
		native: func(t *testing.T) {
			sb, root := oracleWorkspace(t, nil)
			spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.FinishNudgeWindow = rsInt(2); r.MaxIterations = rsInt(4) })
			ns := &nativeScript{turns: [][]llm.ContentPart{
				{structuredCall("c1", "run", map[string]any{"command": "echo ok"})},
				{structuredCall("c2", "read_file", map[string]any{"path": "x1.txt"})},
				{structuredCall("c3", "read_file", map[string]any{"path": "x2.txt"})},
				{structuredCall("c4", "read_file", map[string]any{"path": "x3.txt"})},
			}}
			if _, err := RunNative(context.Background(), spec, Runtime{Model: ns, Sandbox: sb}, Content{Task: "t", Root: root}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(requestsText(ns.calls), "your last build/test run passed") {
				t.Fatal("FinishNudgeWindow=2 with a green run and stable files never fired")
			}
		},
	},
}

func rsBool(v bool) *bool { return &v }

// observeOnlyToolsT filters the default toolset down to the observe-only
// allowlist (isObserveOnly fails closed on `run`, which ReadOnlyTools keeps).
func observeOnlyToolsT(sb sandbox.Sandbox) map[string]Tool {
	tools := ReadOnlyTools(sb, time.Second)
	for name := range tools {
		if !observeOnlyTools[name] {
			delete(tools, name)
		}
	}
	return tools
}

// TestProfileFieldConsumptionOracle is the S6a ship gate: every profile
// FieldID must be classified with a named consumption site, and every
// loop-consumed field's behavioral assertion must pass in each loop that
// consumes it.
func TestProfileFieldConsumptionOracle(t *testing.T) {
	for _, f := range profile.ProfileFields {
		f := f
		entry, ok := consumptionSites[f]
		if !ok {
			t.Errorf("profile field %s has no consumption classification — a new field must name its consuming component here", f)
			continue
		}
		if entry.site == "" {
			t.Errorf("profile field %s has an empty consumption site", f)
			continue
		}
		if entry.builderOnly != "" {
			// Builder-consumed: pin the resolution-side threading only.
			t.Run(string(f)+"/resolution", func(t *testing.T) {
				switch f {
				case profile.MemoryField:
					spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.Memory = rsBool(true) })
					if !spec.Policy().Memory {
						t.Fatal("Memory=true did not reach the resolved policy")
					}
				case profile.WorktreeField:
					spec := oracleResolve(t, func(r *runspec.RequestedConfig) { r.Worktree = rsString("required") })
					if spec.Policy().Worktree != "required" {
						t.Fatal("Worktree=required did not reach the resolved policy")
					}
				default:
					t.Fatalf("builder-only field %s has no resolution pin", f)
				}
			})
			continue
		}
		if entry.text == nil && entry.native == nil {
			t.Errorf("profile field %s is loop-classified but has no behavioral assertion", f)
			continue
		}
		if entry.text != nil {
			t.Run(string(f)+"/Run", func(t *testing.T) { t.Parallel(); entry.text(t) })
		}
		if entry.native != nil {
			t.Run(string(f)+"/RunNative", func(t *testing.T) { t.Parallel(); entry.native(t) })
		}
	}
}
