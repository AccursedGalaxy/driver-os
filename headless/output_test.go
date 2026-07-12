package headless

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
)

// The output layer is the CLI's machine contract (docs/specs/CLI-SCRIPTABLE.md D3/D4). These
// tests pin the guarantees the council hardened: exit-code classes, field
// nullability, the unverified-answer exposure, and valid-JSON-on-every-path.

func buildResult(res *agent.RunResult, model, transcriptPath, baselineCommit string, baselineDirtyFiles []string, artifactPaths ...string) resultObject {
	price := func(model string, usage llm.Usage) (float64, bool) {
		var in, out float64
		switch model {
		case "openai/gpt-5.5":
			in, out = 5, 30
		case "deepseek/deepseek-v4-flash":
			in, out = .09, .18
		default:
			return 0, false
		}
		return float64(usage.PromptTokens)/1e6*in + float64(usage.CompletionTokens)/1e6*out, true
	}
	return buildResultWithPrice(res, model, transcriptPath, baselineCommit, baselineDirtyFiles, price, artifactPaths...)
}

func TestExitCodeFor(t *testing.T) {
	cases := map[agent.Outcome]int{
		agent.Answered:        0,
		agent.Unverified:      2,
		agent.HitCap:          3,
		agent.HitDeadline:     3,
		agent.HitContextLimit: 3,
		agent.KilledRepeat:    4,
		agent.KilledSpiral:    4,
		agent.KilledStagnant:  4,
		agent.ProviderErr:     5,
		agent.RefusedUnsafe:   6,
		agent.Canceled:        7,
		agent.ScopeViolation:  8,
		agent.Outcome("???"):  1, // an unknown outcome must not masquerade as success.
	}
	for outcome, want := range cases {
		if got := exitCodeFor(outcome); got != want {
			t.Errorf("exitCodeFor(%q) = %d, want %d", outcome, got, want)
		}
	}
}

func baseResult(outcome agent.Outcome) *agent.RunResult {
	return &agent.RunResult{
		ID:         "20260604-010203-abc",
		Task:       "do the thing",
		Outcome:    outcome,
		Answer:     "the answer",
		Reason:     "because reasons",
		Iterations: 4,
		Usage:      llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		StartedAt:  time.Date(2026, 6, 4, 1, 2, 3, 0, time.UTC),
		EndedAt:    time.Date(2026, 6, 4, 1, 2, 9, 0, time.UTC),
	}
}

func TestBuildResult_AnswerExposure(t *testing.T) {
	// answered AND unverified both expose the answer (O5); every other outcome nulls it.
	for _, o := range []agent.Outcome{agent.Answered, agent.Unverified} {
		r := buildResult(baseResult(o), "m", "", "", nil)
		if r.Answer == nil || *r.Answer != "the answer" {
			t.Errorf("outcome %q: want answer exposed, got %v", o, r.Answer)
		}
	}
	r := buildResult(baseResult(agent.HitCap), "m", "", "", nil)
	if r.Answer != nil {
		t.Errorf("hit_cap: want answer null, got %q", *r.Answer)
	}
}

func TestBuildResult_ReasonAndError(t *testing.T) {
	// answered carries no reason (the answer is the payload); a non-answer does.
	if got := buildResult(baseResult(agent.Answered), "m", "", "", nil).Reason; got != "" {
		t.Errorf("answered reason = %q, want empty", got)
	}
	if got := buildResult(baseResult(agent.HitCap), "m", "", "", nil).Reason; got == "" {
		t.Error("hit_cap reason should be non-empty")
	}

	// error is an object only for provider_error; null otherwise.
	if buildResult(baseResult(agent.Answered), "m", "", "", nil).Error != nil {
		t.Error("answered should have null error")
	}
	pe := baseResult(agent.ProviderErr)
	pe.Err = errString("502 bad gateway")
	r := buildResult(pe, "m", "", "", nil)
	if r.Error == nil || r.Error.Kind != "provider_error" || r.Error.Message != "502 bad gateway" {
		t.Errorf("provider_error: want {provider_error, 502 bad gateway}, got %+v", r.Error)
	}
}

func TestBuildResult_TranscriptPathNullability(t *testing.T) {
	if p := buildResult(baseResult(agent.Answered), "m", "", "", nil).TranscriptPath; p != nil {
		t.Errorf("empty path should serialize to null, got %q", *p)
	}
	r := buildResult(baseResult(agent.Answered), "m", "/runs/x.json", "", nil)
	if r.TranscriptPath == nil || *r.TranscriptPath != "/runs/x.json" {
		t.Errorf("want transcript path set, got %v", r.TranscriptPath)
	}
}

func TestShouldRemovePatchedWorktree(t *testing.T) {
	cases := []struct {
		name    string
		outcome agent.Outcome
		review  bool
		keep    bool
		want    bool
	}{
		{name: "answered banks patch and removes worktree", outcome: agent.Answered, want: true},
		{name: "hit cap banks patch and removes worktree", outcome: agent.HitCap, want: true},
		{name: "unverified banks patch and removes worktree", outcome: agent.Unverified, want: true},
		{name: "review keeps live worktree for final diff", outcome: agent.HitCap, review: true, want: false},
		{name: "keep flag keeps worktree for inspection", outcome: agent.Answered, keep: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRemovePatchedWorktree(tc.outcome, tc.review, tc.keep); got != tc.want {
				t.Fatalf("shouldRemovePatchedWorktree(%q, review=%t, keep=%t) = %t, want %t", tc.outcome, tc.review, tc.keep, got, tc.want)
			}
		})
	}
}

func TestCollectPatchErrorMessageMissingWorktreeIsLoud(t *testing.T) {
	err := os.ErrNotExist
	msg := collectPatchErrorMessage("/tmp/driver-agent-wt-missing", err)
	for _, want := range []string{"worktree directory vanished mid-run", "likely external interference", "concurrent worktree sweep", "run's work is lost"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in message: %s", want, msg)
		}
	}
}

// TestBuildResult_BaselineCommitNullability checks that baseline_commit is
// always present: non-null when -worktree is on (baseline != ""), null when off.
func TestBuildResult_BaselineCommitNullability(t *testing.T) {
	// Non-worktree: baselineCommit is empty → null in JSON.
	r := buildResult(baseResult(agent.Answered), "m", "", "", nil)
	if r.BaselineCommit != nil {
		t.Fatalf("non-worktree BaselineCommit should be nil, got %q", *r.BaselineCommit)
	}
	if r.BaselineDirtyFiles != nil {
		t.Fatalf("non-worktree BaselineDirtyFiles should be nil, got %v", r.BaselineDirtyFiles)
	}
	// Worktree with clean checkout: baselineCommit set, no dirty files.
	r = buildResult(baseResult(agent.Answered), "m", "", "abc1234", nil)
	if r.BaselineCommit == nil || *r.BaselineCommit != "abc1234" {
		t.Fatalf("want BaselineCommit = abc1234, got %v", r.BaselineCommit)
	}
	if r.BaselineDirtyFiles != nil {
		t.Fatalf("clean BaselineDirtyFiles should be nil, got %v", r.BaselineDirtyFiles)
	}
	// Worktree with dirty checkout: both baselineCommit and dirty files.
	r = buildResult(baseResult(agent.Answered), "m", "", "snap1", []string{"a.go", "b.go"})
	if r.BaselineCommit == nil || *r.BaselineCommit != "snap1" {
		t.Fatalf("want BaselineCommit = snap1, got %v", r.BaselineCommit)
	}
	if len(r.BaselineDirtyFiles) != 2 || r.BaselineDirtyFiles[0] != "a.go" || r.BaselineDirtyFiles[1] != "b.go" {
		t.Fatalf("want BaselineDirtyFiles = [a.go b.go], got %v", r.BaselineDirtyFiles)
	}
}

func TestBuildResult_WorktreeArtifactNullability(t *testing.T) {
	if r := buildResult(baseResult(agent.Answered), "m", "", "", nil); r.WorktreePath != nil || r.PatchPath != nil {
		t.Fatalf("empty artifact paths should serialize to null, got worktree=%v patch=%v", r.WorktreePath, r.PatchPath)
	}
	r := buildResult(baseResult(agent.Answered), "m", "", "", nil, "/tmp/wt", "/runs/id.patch")
	if r.WorktreePath == nil || *r.WorktreePath != "/tmp/wt" {
		t.Fatalf("want worktree path set, got %v", r.WorktreePath)
	}
	if r.PatchPath == nil || *r.PatchPath != "/runs/id.patch" {
		t.Fatalf("want patch path set, got %v", r.PatchPath)
	}
}

func TestBuildResult_PatchErrorForcesSetupExitAndJSONField(t *testing.T) {
	r := buildResult(baseResult(agent.Answered), "m", "", "", nil, "/tmp/wt", "", "collect failed")
	if r.PatchError != "collect failed" {
		t.Fatalf("PatchError = %q, want collect failed", r.PatchError)
	}
	if r.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", r.ExitCode)
	}
	var out bytes.Buffer
	if code := emitResult(&out, formatJSON, r); code != 1 {
		t.Fatalf("emitResult exit code = %d, want 1", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("emitted invalid JSON: %v\n%s", err, out.String())
	}
	if got := m["patch_error"]; got != "collect failed" {
		t.Fatalf("json patch_error = %#v, want collect failed", got)
	}
	if got := m["exit_code"]; got != float64(1) {
		t.Fatalf("json exit_code = %#v, want 1", got)
	}
}

// TestEmitResultJSON checks the json mode emits one valid object with every
// promised key present (O8) and the nullable ones rendered as JSON null.
func TestEmitResultJSON(t *testing.T) {
	var out bytes.Buffer
	code := emitResult(&out, formatJSON, buildResult(baseResult(agent.HitCap), "gpt-x", "/runs/x.json", "", nil))
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("emitted invalid JSON: %v\n%s", err, out.String())
	}
	for _, k := range []string{"schema_version", "id", "outcome", "exit_code", "task",
		"model", "answer", "reason", "iterations", "usage", "started_at", "ended_at",
		"transcript_path", "worktree_path", "patch_path", "patch_error", "error", "review", "plan",
		"baseline_commit",
		"solver_cost_usd", "reviewer_cost_usd", "planner_cost_usd", "total_cost_usd", "cost_source"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q (all top-level keys must always be present)", k)
		}
	}
	if m["answer"] != nil { // hit_cap → null answer.
		t.Errorf("hit_cap answer should be null, got %v", m["answer"])
	}
	if m["error"] != nil {
		t.Errorf("hit_cap error should be null, got %v", m["error"])
	}
	if m["exit_code"].(float64) != 3 {
		t.Errorf("exit_code field = %v, want 3", m["exit_code"])
	}
}

// TestEmitResultText checks text mode puts the answer then the SUMMARY line on the
// data channel, and returns the outcome's exit code.
func TestEmitResultText(t *testing.T) {
	var out bytes.Buffer
	code := emitResult(&out, formatText, buildResult(baseResult(agent.Answered), "m", "", "", nil))
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	s := out.String()
	if !strings.HasPrefix(s, "the answer\n") {
		t.Errorf("text output should lead with the answer, got:\n%s", s)
	}
	if !strings.Contains(s, "SUMMARY outcome=answered id=20260604-010203-abc iters=4") {
		t.Errorf("missing/garbled SUMMARY line:\n%s", s)
	}
}

func TestBuildResult_Cost(t *testing.T) {
	// Use a model from eval.Pricing: "openai/gpt-5.5" {In: 5.00, Out: 30.00}
	// Usage: 10 prompt, 5 completion.
	// Cost = (10/1e6 * 5.00) + (5/1e6 * 30.00) = 0.00005 + 0.00015 = 0.0002
	model := "openai/gpt-5.5"
	res := baseResult(agent.Answered)
	res.Usage = llm.Usage{PromptTokens: 10, CompletionTokens: 5}

	t.Run("PricedSolverOnly", func(t *testing.T) {
		r := buildResult(res, model, "", "", nil)
		if r.SolverCost == nil || *r.SolverCost != 0.0002 {
			t.Errorf("solver cost = %v, want 0.0002", r.SolverCost)
		}
		if r.ReviewerCost != nil || r.PlannerCost != nil {
			t.Errorf("unconfigured roles should be null, got R=%v P=%v", r.ReviewerCost, r.PlannerCost)
		}
		if r.TotalCost == nil || *r.TotalCost != 0.0002 {
			t.Errorf("total cost = %v, want 0.0002", r.TotalCost)
		}
	})

	t.Run("SolverAndReviewerPriced", func(t *testing.T) {
		res.Review = &agent.ReviewReport{
			ReviewerModel: "deepseek/deepseek-v4-flash", // {In: 0.09, Out: 0.18}
			Usage:         llm.Usage{PromptTokens: 100, CompletionTokens: 50},
		}
		// Reviewer cost = (100/1e6 * 0.09) + (50/1e6 * 0.18) = 0.000009 + 0.000009 = 0.000018
		// Total = 0.0002 + 0.000018 = 0.000218
		r := buildResult(res, model, "", "", nil)
		if r.ReviewerCost == nil || *r.ReviewerCost != 0.000018 {
			t.Errorf("reviewer cost = %v, want 0.000018", r.ReviewerCost)
		}
		if r.TotalCost == nil || *r.TotalCost != 0.000218 {
			t.Errorf("total cost = %v, want 0.000218", r.TotalCost)
		}
	})

	t.Run("ReviewerUnpriced", func(t *testing.T) {
		res.Review = &agent.ReviewReport{
			ReviewerModel: "unknown-model",
			Usage:         llm.Usage{PromptTokens: 100, CompletionTokens: 50},
		}
		r := buildResult(res, model, "", "", nil)
		if r.ReviewerCost != nil {
			t.Errorf("unpriced reviewer should be null, got %v", *r.ReviewerCost)
		}
		if r.TotalCost != nil {
			t.Errorf("total cost should be null if any role is unpriced, got %v", *r.TotalCost)
		}
		// Solver cost should still be there.
		if r.SolverCost == nil || *r.SolverCost != 0.0002 {
			t.Errorf("solver cost should still be priced, got %v", r.SolverCost)
		}
	})

	t.Run("ReviewerModelEmptyButUsagePresent", func(t *testing.T) {
		res.Review = &agent.ReviewReport{
			ReviewerModel: "",
			Usage:         llm.Usage{PromptTokens: 100, CompletionTokens: 50},
		}
		r := buildResult(res, model, "", "", nil)
		if r.TotalCost != nil {
			t.Errorf("total cost should be null if reviewer ran (usage > 0) but model is unknown, got %v", *r.TotalCost)
		}
	})

	t.Run("ReviewerModelEmptyButTotalTokensOnly", func(t *testing.T) {
		res.Review = &agent.ReviewReport{
			ReviewerModel: "",
			Usage:         llm.Usage{TotalTokens: 15},
		}
		r := buildResult(res, model, "", "", nil)
		if r.TotalCost != nil {
			t.Errorf("total cost should be null if reviewer ran (TotalTokens > 0) but model is unknown, got %v", *r.TotalCost)
		}
	})

	t.Run("MeteredPreferred", func(t *testing.T) {
		res.Review = nil
		res.Usage = llm.Usage{PromptTokens: 10, CompletionTokens: 5, Cost: 0.001}
		r := buildResult(res, model, "", "", nil)
		if r.SolverCost == nil || *r.SolverCost != 0.001 {
			t.Errorf("solver cost = %v, want 0.001 (metered)", r.SolverCost)
		}
		if r.CostSource == nil || *r.CostSource != "metered" {
			t.Errorf("cost source = %v, want metered", r.CostSource)
		}
	})

	t.Run("FallbackToTable", func(t *testing.T) {
		res.Usage = llm.Usage{PromptTokens: 10, CompletionTokens: 5, Cost: 0}
		r := buildResult(res, model, "", "", nil)
		if r.SolverCost == nil || *r.SolverCost != 0.0002 {
			t.Errorf("solver cost = %v, want 0.0002 (modeled)", r.SolverCost)
		}
		if r.CostSource == nil || *r.CostSource != "modeled" {
			t.Errorf("cost source = %v, want modeled", r.CostSource)
		}
	})

	t.Run("MixedLabeling", func(t *testing.T) {
		res.Usage = llm.Usage{PromptTokens: 10, CompletionTokens: 5, Cost: 0.001}
		res.Review = &agent.ReviewReport{
			ReviewerModel: "deepseek/deepseek-v4-flash",
			Usage:         llm.Usage{PromptTokens: 100, CompletionTokens: 50, Cost: 0}, // modeled
		}
		r := buildResult(res, model, "", "", nil)
		if r.CostSource == nil || *r.CostSource != "mixed" {
			t.Errorf("cost source = %v, want mixed", r.CostSource)
		}
	})
}

func TestValidateFormat(t *testing.T) {
	for _, ok := range []string{"text", "json", "ndjson"} {
		if _, err := validateFormat(ok); err != nil {
			t.Errorf("validateFormat(%q) errored: %v", ok, err)
		}
	}
	if _, err := validateFormat("yaml"); err == nil {
		t.Error("unknown format should error")
	}
}

func TestValidateProtocol(t *testing.T) {
	for _, ok := range []string{"tools", "text"} {
		if err := validateProtocol(ok); err != nil {
			t.Errorf("validateProtocol(%q) errored: %v", ok, err)
		}
	}
	// The motivating typo (review #11): -protocol=tool used to silently fall
	// back to the text loop instead of failing loudly.
	if err := validateProtocol("tool"); err == nil {
		t.Error("a -protocol typo must be a setup error, not a silent text-loop fallback")
	}
}

// decodeNDJSON splits an ndjson stream into one decoded object per line, asserting
// every line is valid JSON (the core stream invariant).
func decodeNDJSON(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("ndjson line is not valid JSON: %v\n%q", err, line)
		}
		out = append(out, m)
	}
	return out
}

// TestNDJSONObserverEvents checks each Observer call maps to its typed event with
// the promised shape, and that observation text is emitted WHOLE (not oneLined).
func TestNDJSONObserverEvents(t *testing.T) {
	var out bytes.Buffer
	o := ndjsonObserver{&out}
	o.Iteration(1, 8)
	o.Model("read_file main.go")
	o.Observation("exit 0 (12ms)\nstdout:\nhi") // contains a newline on purpose.
	o.Note("near cap — nudging to finish")
	o.Done("the answer")

	evs := decodeNDJSON(t, out.String())
	if len(evs) != 5 {
		t.Fatalf("want 5 events, got %d", len(evs))
	}
	if evs[0]["type"] != "iteration" || evs[0]["i"].(float64) != 1 || evs[0]["max"].(float64) != 8 {
		t.Errorf("iteration event wrong: %v", evs[0])
	}
	if evs[1]["type"] != "model" || evs[1]["text"] != "read_file main.go" {
		t.Errorf("model event wrong: %v", evs[1])
	}
	// The observation must survive whole, embedded newline included (not oneLined).
	if evs[2]["type"] != "observation" || evs[2]["text"] != "exit 0 (12ms)\nstdout:\nhi" {
		t.Errorf("observation event should carry full text, got: %v", evs[2])
	}
	if evs[3]["type"] != "note" {
		t.Errorf("note event wrong: %v", evs[3])
	}
	if evs[4]["type"] != "done" || evs[4]["answer"] != "the answer" {
		t.Errorf("done event wrong: %v", evs[4])
	}
}

// TestEmitResultNDJSON checks the terminal event nests the D3 object under
// `result` (O6) and carries the right exit code.
func TestEmitResultNDJSON(t *testing.T) {
	var out bytes.Buffer
	code := emitResult(&out, formatNDJSON, buildResult(baseResult(agent.Answered), "m", "/runs/x.json", "", nil))
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	evs := decodeNDJSON(t, out.String())
	if len(evs) != 1 || evs[0]["type"] != "result" {
		t.Fatalf("want one terminal result event, got %v", evs)
	}
	r, ok := evs[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("result must be a nested object, got %T", evs[0]["result"])
	}
	if r["outcome"] != "answered" || r["answer"] != "the answer" || r["schema_version"].(float64) != schemaVersion {
		t.Errorf("nested result object wrong: %v", r)
	}
	if _, collides := evs[0]["outcome"]; collides {
		t.Error("result fields must be nested, not flattened into the event (O6)")
	}
}

func TestFailSetupAppendsLedgerRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRIVER_OS_LEDGER_DIR", dir)
	configureSetupLedger(true, "openai/gpt-5.5", "fix setup")
	t.Cleanup(func() { configureSetupLedger(false, "", "") })

	var stdout, stderr bytes.Buffer
	if code := failSetup(&stdout, &stderr, formatJSON, "no_provider_key", "no key"); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"outcome": "setup_error", "exit_code": float64(1), "model": "openai/gpt-5.5", "has_patch": false} {
		if m[k] != want {
			t.Errorf("record[%q] = %v, want %v", k, m[k], want)
		}
	}
	for _, k := range []string{"solver_cost_usd", "reviewer_cost_usd", "planner_cost_usd", "selector_cost_usd", "total_cost_usd"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s should be present and null, got %v (present=%v)", k, v, ok)
		}
	}
}

// TestFailSetupNDJSON checks a setup error is delivered as the terminal result
// event so the stream contract holds on the error path (O9).
func TestFailSetupNDJSON(t *testing.T) {
	var so, se bytes.Buffer
	if code := failSetup(&so, &se, formatNDJSON, "no_provider_key", "no key"); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	evs := decodeNDJSON(t, so.String())
	if len(evs) != 1 || evs[0]["type"] != "result" {
		t.Fatalf("setup error should be a single result event, got %v", evs)
	}
	r := evs[0]["result"].(map[string]any)
	if r["outcome"] != "cli_error" || r["exit_code"].(float64) != 1 {
		t.Errorf("wrapped cli_error wrong: %v", r)
	}
	if se.Len() != 0 {
		t.Errorf("ndjson mode should not write to stderr, got %q", se.String())
	}
}

// TestFailSetup verifies the two channels: json mode writes a valid cli_error
// object to stdout (so a consumer parses unconditionally — O2); text mode writes
// to stderr and leaves stdout empty.
func TestFailSetup(t *testing.T) {
	var so, se bytes.Buffer
	if code := failSetup(&so, &se, formatJSON, "no_provider_key", "no key"); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	var m map[string]any
	if err := json.Unmarshal(so.Bytes(), &m); err != nil {
		t.Fatalf("json cli_error not valid JSON: %v\n%s", err, so.String())
	}
	if m["outcome"] != "cli_error" || m["exit_code"].(float64) != 1 {
		t.Errorf("cli_error object wrong: %v", m)
	}
	if e, ok := m["error"].(map[string]any); !ok || e["kind"] != "no_provider_key" {
		t.Errorf("cli_error.error wrong: %v", m["error"])
	}
	if se.Len() != 0 {
		t.Errorf("json mode should not write to stderr, got %q", se.String())
	}

	so.Reset()
	se.Reset()
	failSetup(&so, &se, formatText, "no_provider_key", "no key")
	if so.Len() != 0 {
		t.Errorf("text mode should leave stdout empty, got %q", so.String())
	}
	if !strings.Contains(se.String(), "no key") {
		t.Errorf("text mode should write the message to stderr, got %q", se.String())
	}
}

// errString is a tiny error with a fixed message for the provider_error case.
type errString string

func (e errString) Error() string { return string(e) }
