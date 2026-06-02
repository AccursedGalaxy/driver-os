package agent

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// nativeScript is a deterministic tool-calling provider: the i-th Generate returns
// the i-th turn's content parts (clamping to the last), recording every Request so
// a test can assert what the loop fed back. Capabilities advertises Tools so it
// stands in for a native provider.
type nativeScript struct {
	turns [][]llm.ContentPart
	calls []llm.Request
	n     int
}

func (s *nativeScript) Name() string                   { return "native" }
func (s *nativeScript) Capabilities() llm.Capabilities { return llm.Capabilities{Tools: true} }
func (s *nativeScript) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("native")
}

func (s *nativeScript) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.calls = append(s.calls, req)
	i := s.n
	if i >= len(s.turns) {
		i = len(s.turns) - 1
	}
	s.n++
	parts := s.turns[i]
	fr := llm.FinishStop
	for _, p := range parts {
		if _, ok := p.(llm.ToolCallPart); ok {
			fr = llm.FinishToolUse
		}
	}
	return &llm.Response{Content: parts, FinishReason: fr, Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
}

// structuredCall builds a tool call with TYPED, multi-field JSON args — the way
// the model calls a structured-schema tool. fields marshals straight into the
// call's Args (real newlines in a string field survive: that's the native point).
func structuredCall(id, name string, fields map[string]any) llm.ToolCallPart {
	args, _ := json.Marshal(fields)
	return llm.ToolCallPart{ID: id, Name: name, Args: args}
}

// toolCall builds a bridge-schema tool call ({"arg": <arg>}) — for tools that
// expose only Run and rely on the native loop's single-string bridge.
func toolCall(id, name, arg string) llm.ToolCallPart {
	args, _ := json.Marshal(map[string]string{"arg": arg})
	return llm.ToolCallPart{ID: id, Name: name, Args: args}
}

// runNative runs the loop against a fixture sandbox and returns the result, the
// script (to assert the round-trip), and the sandbox (to read state back).
func runNative(t *testing.T, files map[string]string, turns [][]llm.ContentPart) (*RunResult, *nativeScript, sandbox.Sandbox) {
	t.Helper()
	ns := &nativeScript{turns: turns}
	sb := sbWith(t, files)
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "test task"})
	if err != nil {
		t.Fatalf("RunNative error: %v", err)
	}
	return res, ns, sb
}

func readback(t *testing.T, sb sandbox.Sandbox, path string) string {
	t.Helper()
	data, err := sb.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("read-back %q: %v", path, err)
	}
	return string(data)
}

func TestRunNativeToolThenAnswer(t *testing.T) {
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "read_file", map[string]any{"path": "go.mod"})}, // call a tool
		{llm.Text("module is stresstest")},                                    // then finish in prose
	}
	res, ns, _ := runNative(t, map[string]string{"go.mod": "module x\n"}, turns)

	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if res.Answer != "module is stresstest" {
		t.Errorf("Answer = %q", res.Answer)
	}
	if len(res.Steps) < 2 || res.Steps[0].Verb != "read_file" || res.Steps[0].Observation == "" {
		t.Errorf("expected a read_file observation step, got %+v", res.Steps)
	}
	// The 2nd request must carry the tool RESULT back to the model (the round-trip).
	sawToolResult := false
	for _, m := range ns.calls[1].Messages {
		if m.Role == llm.RoleTool {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Errorf("2nd request did not carry a tool-result message")
	}
	// And the tools were advertised on the request, with a TYPED schema (not the
	// single-string bridge): read_file must declare a `path` property.
	if len(ns.calls[0].Tools) == 0 {
		t.Fatalf("request did not advertise any tools")
	}
	if !schemaHasProperty(t, ns.calls[0].Tools, "read_file", "path") {
		t.Errorf("read_file schema is not the structured one (no `path` property)")
	}
}

// schemaHasProperty reports whether the advertised tool `name` declares a
// top-level property `prop` in its JSON schema — the test that the native loop
// advertised the structured schema, not the bridge `arg`.
func schemaHasProperty(t *testing.T, tools []llm.Tool, name, prop string) bool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name != name {
			continue
		}
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tl.Schema, &s); err != nil {
			t.Fatalf("schema for %q is not valid JSON: %v", name, err)
		}
		_, ok := s.Properties[prop]
		return ok
	}
	t.Fatalf("tool %q not advertised", name)
	return false
}

func TestRunNativeTextOnlyIsImmediateAnswer(t *testing.T) {
	res, _, _ := runNative(t, nil, [][]llm.ContentPart{{llm.Text("done already")}})
	if res.Outcome != Answered || res.Answer != "done already" {
		t.Errorf("Outcome=%q Answer=%q, want Answered/'done already'", res.Outcome, res.Answer)
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", res.Iterations)
	}
}

func TestRunNativeToolErrorIsObservation(t *testing.T) {
	// A failing tool must not crash the loop: it becomes an ERROR observation and
	// the run continues (P6). Since no tool succeeded, the run is NOT grounded.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "read_file", map[string]any{"path": "nope.txt"})},
		{llm.Text("file is missing")},
	}
	res, _, _ := runNative(t, nil, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q, want Answered", res.Outcome)
	}
	if !strings.Contains(res.Steps[0].Observation, "ERROR") {
		t.Errorf("expected an ERROR observation, got %q", res.Steps[0].Observation)
	}
	if res.Steps[0].Grounded {
		t.Errorf("a failed tool must not ground the run")
	}
}

func TestRunNativeWriteMultilineStructured(t *testing.T) {
	// The headline payoff: structured args carry a real multi-line `content`
	// string — no "\n" escaping AND no `path content` splitting. The model fills
	// two typed fields and the file lands byte-for-byte.
	content := "line one\nline two\nline three"
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "out.txt", "content": content})},
		{llm.Text("written")},
	}
	res, _, sb := runNative(t, nil, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Steps[0].Observation, "3 line(s)") {
		t.Errorf("write observation = %q, want 3 lines written", res.Steps[0].Observation)
	}
	if got := readback(t, sb, "out.txt"); got != content {
		t.Errorf("file content = %q, want %q", got, content)
	}
}

func TestRunNativeWriteBackslashContentVerbatim(t *testing.T) {
	// The single most important guarantee of the structured path: content is
	// written BYTE-FOR-BYTE. Real code routinely carries backslashes — regexes,
	// escape sequences, Windows paths. The text loop's `unescape` would turn `\t`
	// into a tab and `\n` into a newline and REJECT `\d` as a bad escape; the
	// structured RunJSON path must do NONE of that. This is the regression guard
	// against ever re-routing the structured write back through unescape.
	content := `regex := "\d+\.\d+"` + "\n" + // \d would be REJECTED by unescape
		`win := "C:\tmp\new"` + "\n" + // \t, \n would be CORRUPTED by unescape
		`esc := "a\\b\nc"` + "\n" // \\ would be collapsed by unescape
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "code.go", "content": content})},
		{llm.Text("written")},
	}
	res, _, sb := runNative(t, nil, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if got := readback(t, sb, "code.go"); got != content {
		t.Errorf("backslash content corrupted:\n got = %q\nwant = %q", got, content)
	}
}

func TestRunNativeEditFileBackslashContentVerbatim(t *testing.T) {
	// The same verbatim guarantee for edit_file's replacement content (a distinct
	// op from write_file): backslash sequences in the structured `content` field
	// must splice in untouched, never decoded by unescape.
	start := "a\nOLD\nc\n"
	repl := `re := "\d+"` + "\t" + `\\ literal \n here` // literal backslashes + a real tab
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "edit_file", map[string]any{"path": "f.txt", "from": 2, "to": 2, "content": repl})},
		{llm.Text("ok")},
	}
	_, _, sb := runNative(t, map[string]string{"f.txt": start}, turns)
	want := "a\n" + repl + "\nc\n"
	if got := readback(t, sb, "f.txt"); got != want {
		t.Errorf("edit backslash content corrupted:\n got = %q\nwant = %q", got, want)
	}
}

func TestNativeSchemasUseBehaviorOnlyDescription(t *testing.T) {
	// Fix A: the native loop must advertise the behavior-only NativeDesc, NOT the
	// text-protocol Desc. The Desc tells the model to "write a line break as the
	// two characters \n" — true for the one-line text protocol, a CORRUPTION trap
	// in native mode (the structured content is written verbatim, so a literal
	// `\n` lands as backslash-n). The tool-level native description must carry no
	// such escape/positional-arg framing; the per-field schema owns the format.
	tools := DefaultTools(sbWith(t, nil), defaultRunTimeout)
	byName := map[string]llm.Tool{}
	for _, s := range nativeSchemas(tools) {
		byName[s.Name] = s
	}
	for _, name := range []string{"write_file", "edit_file", "read_file"} {
		d := byName[name].Description
		if strings.Contains(d, `\n`) || strings.Contains(d, `\t`) {
			t.Errorf("native %s description leaks \\n/\\t escape framing: %q", name, d)
		}
		if strings.Contains(strings.ToLower(d), "same line") || strings.Contains(d, ":<from>-<to>") {
			t.Errorf("native %s description leaks one-line/positional-arg framing: %q", name, d)
		}
		if d == "" || d == tools[name].Desc {
			t.Errorf("native %s description should be the distinct behavior-only NativeDesc, got %q", name, d)
		}
	}
}

func TestRunNativeReadFileStructuredRange(t *testing.T) {
	// {path, from, to} returns exactly the absolute-numbered slice.
	file := "a\nb\nc\nd\ne\n"
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "read_file", map[string]any{"path": "f.txt", "from": 2, "to": 4})},
		{llm.Text("done")},
	}
	res, _, _ := runNative(t, map[string]string{"f.txt": file}, turns)
	obs := res.Steps[0].Observation
	if !strings.Contains(obs, "2| b") || !strings.Contains(obs, "4| d") {
		t.Errorf("range read = %q, want lines 2-4", obs)
	}
	if strings.Contains(obs, "1| a") || strings.Contains(obs, "5| e") {
		t.Errorf("range read leaked lines outside 2-4: %q", obs)
	}
}

func TestRunNativeReadFileStructuredToWithoutFrom(t *testing.T) {
	// Fix B: {path, to:N} with no `from` reads the first N lines (from defaults to
	// 1), rather than silently ignoring `to` and dumping the whole file.
	file := "a\nb\nc\nd\ne\n"
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "read_file", map[string]any{"path": "f.txt", "to": 2})},
		{llm.Text("done")},
	}
	res, _, _ := runNative(t, map[string]string{"f.txt": file}, turns)
	obs := res.Steps[0].Observation
	if !strings.Contains(obs, "1| a") || !strings.Contains(obs, "2| b") {
		t.Errorf("to-without-from read = %q, want lines 1-2", obs)
	}
	if strings.Contains(obs, "3| c") {
		t.Errorf("to-without-from leaked past line 2: %q", obs)
	}
}

func TestRunNativeEditFileStructuredReplace(t *testing.T) {
	// {path, from, to, content} replaces exactly the named lines.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "edit_file", map[string]any{"path": "f.txt", "from": 2, "to": 2, "content": "BB"})},
		{llm.Text("done")},
	}
	res, _, sb := runNative(t, map[string]string{"f.txt": "a\nb\nc\n"}, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if got, want := readback(t, sb, "f.txt"), "a\nBB\nc\n"; got != want {
		t.Errorf("after replace = %q, want %q", got, want)
	}
	// No `:from-to` substring anywhere in the trace's recorded arg (it's typed JSON).
	if strings.Contains(res.Steps[0].Arg, ":2-2") {
		t.Errorf("Step.Arg carries a positional range substring: %q", res.Steps[0].Arg)
	}
}

func TestRunNativeEditFileStructuredDelete(t *testing.T) {
	// content OMITTED deletes the range (the *string pointer is nil).
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "edit_file", map[string]any{"path": "f.txt", "from": 2, "to": 2})},
		{llm.Text("done")},
	}
	_, _, sb := runNative(t, map[string]string{"f.txt": "a\nb\nc\n"}, turns)
	if got, want := readback(t, sb, "f.txt"), "a\nc\n"; got != want {
		t.Errorf("after delete = %q, want %q", got, want)
	}
}

func TestRunNativeEditFileStructuredToEOF(t *testing.T) {
	// `to` OMITTED edits from `from` through end-of-file.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "edit_file", map[string]any{"path": "f.txt", "from": 2, "content": "X\nY"})},
		{llm.Text("done")},
	}
	_, _, sb := runNative(t, map[string]string{"f.txt": "a\nb\nc\nd\n"}, turns)
	if got, want := readback(t, sb, "f.txt"), "a\nX\nY\n"; got != want {
		t.Errorf("after edit-to-EOF = %q, want %q", got, want)
	}
}

func TestRunNativeRespectsMaxIterations(t *testing.T) {
	// Never answers (distinct non-list_dir calls dodge both detectors) -> hits cap.
	turns := [][]llm.ContentPart{
		{structuredCall("a", "read_file", map[string]any{"path": "a"})},
		{structuredCall("b", "read_file", map[string]any{"path": "b"})},
		{structuredCall("c", "read_file", map[string]any{"path": "c"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, nil), Task: "t", MaxIterations: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != HitCap || res.Iterations != 2 {
		t.Errorf("Outcome=%q Iterations=%d, want HitCap/2", res.Outcome, res.Iterations)
	}
}

func TestRunNativeBridgeFallbackForRunOnlyTool(t *testing.T) {
	// A custom Tool with only Run (no Schema/RunJSON) must still dispatch in native
	// mode via the single-string `arg` bridge — so external toolsets keep working.
	var gotArg string
	tools := map[string]Tool{
		"echo": {
			Name: "echo",
			Desc: "echo the arg back",
			Run: func(_ context.Context, arg string) (string, error) {
				gotArg = arg
				return "echoed: " + arg, nil
			},
		},
	}
	turns := [][]llm.ContentPart{
		{toolCall("c1", "echo", "hello world")},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, nil), Tools: tools, Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if gotArg != "hello world" {
		t.Errorf("bridge arg = %q, want 'hello world'", gotArg)
	}
	if res.Steps[0].Observation != "echoed: hello world" {
		t.Errorf("observation = %q", res.Steps[0].Observation)
	}
	// The advertised schema for a bridge tool is the single-`arg` shape.
	if !schemaHasProperty(t, ns.calls[0].Tools, "echo", "arg") {
		t.Errorf("bridge tool should advertise an `arg` property")
	}
}

func TestRunNativeParallelListDirIsNotSpiral(t *testing.T) {
	// noProgressWindow list_dir calls, but all in ONE turn (legitimate parallel
	// fan-out the native channel enables) — that is a single exploration step, not
	// a spiral. The detector must NOT kill it: it counts list_dir-only TURNS, not
	// individual calls.
	files := map[string]string{"a/x": "1", "b/x": "1", "c/x": "1", "d/x": "1"}
	turns := [][]llm.ContentPart{
		{
			structuredCall("1", "list_dir", map[string]any{"path": "a"}),
			structuredCall("2", "list_dir", map[string]any{"path": "b"}),
			structuredCall("3", "list_dir", map[string]any{"path": "c"}),
			structuredCall("4", "list_dir", map[string]any{"path": "d"}),
		},
		{llm.Text("explored all four")},
	}
	res, _, _ := runNative(t, files, turns)
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered — a parallel list_dir fan-out is not a spiral", res.Outcome, res.Reason)
	}
}

func TestRunNativeListDirSpiralAcrossTurns(t *testing.T) {
	// The spiral detector still fires on the real thing: noProgressWindow
	// list_dir-ONLY turns in a row (each a different path), never escalating.
	files := map[string]string{"a/x": "1", "b/x": "1", "c/x": "1", "d/x": "1"}
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("2", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("3", "list_dir", map[string]any{"path": "c"})},
		{structuredCall("4", "list_dir", map[string]any{"path": "d"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Errorf("Outcome = %q (%s), want KilledSpiral", res.Outcome, res.Reason)
	}
}

func TestRunNativeMixedTurnResetsSpiral(t *testing.T) {
	// A turn that mixes list_dir with another tool is progress: it resets the
	// nav-turn count, so list_dir, list_dir, (list_dir+read_file), list_dir does
	// NOT trip the spiral even though four turns touch list_dir.
	files := map[string]string{"a/x": "1", "b/x": "1", "c/x": "1", "d/x": "1", "f.txt": "hi\n"}
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("2", "list_dir", map[string]any{"path": "b"})},
		{
			structuredCall("3a", "list_dir", map[string]any{"path": "c"}),
			structuredCall("3b", "read_file", map[string]any{"path": "f.txt"}),
		},
		{structuredCall("4", "list_dir", map[string]any{"path": "d"})},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered — a mixed turn resets the spiral count", res.Outcome, res.Reason)
	}
}

// failRun is a `run` call whose command fails deterministically with identical
// output every time — the raw material for the stagnant-observation detector.
func failRun(id string) llm.ToolCallPart {
	return structuredCall(id, "run", map[string]any{"command": "echo boom 1>&2; exit 2"})
}

func TestRunNativeVerifyCmdFailMarksUnverified(t *testing.T) {
	// The headline A1 fix: the model writes a file then CLAIMS success in prose,
	// but the caller-named verification command fails -> the run is Unverified, not
	// a false Answered/exit-0. This is the DOGFOOD R9/R10 termination-by-silence catch.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "f.txt", "content": "hi"})},
		{llm.Text("done — all tests pass")}, // the false claim.
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, nil), Task: "t", VerifyCmd: "exit 1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Unverified {
		t.Fatalf("Outcome = %q (%s), want Unverified", res.Outcome, res.Reason)
	}
	if res.Answer != "done — all tests pass" {
		t.Errorf("Answer = %q, want the model's prose preserved", res.Answer)
	}
	if !strings.Contains(res.Reason, "verification command") {
		t.Errorf("Reason = %q, want it to name the failing verification command", res.Reason)
	}
}

func TestRunNativeVerifyCmdPassStaysAnswered(t *testing.T) {
	// When the verification command passes, the run is a genuine Answered.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "f.txt", "content": "hi"})},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, nil), Task: "t", VerifyCmd: "exit 0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered — verification passed", res.Outcome, res.Reason)
	}
}

func TestRunNativeVerifyLastRunFallback(t *testing.T) {
	// Without a VerifyCmd, the opt-in last-run heuristic marks a silent finish that
	// follows a still-failing run as Unverified; and with the flag OFF (default) the
	// same run is accepted as Answered (so absence/grep answers don't regress).
	turns := [][]llm.ContentPart{
		{failRun("r1")},       // most recent run is a failure...
		{llm.Text("all set")}, // ...then the model claims done.
	}
	for _, tc := range []struct {
		flag bool
		want Outcome
	}{
		{true, Unverified},
		{false, Answered},
	} {
		ns := &nativeScript{turns: turns}
		res, err := RunNative(context.Background(), Config{
			Model: ns, Sandbox: sbWith(t, nil), Task: "t", VerifyLastRun: tc.flag,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != tc.want {
			t.Errorf("VerifyLastRun=%v: Outcome = %q (%s), want %q", tc.flag, res.Outcome, res.Reason, tc.want)
		}
	}
}

func TestRunNativeVerifyContinueRecoversPrematureFinish(t *testing.T) {
	// The pass-rate lever: a model finishes PREMATURELY (turn 1 is a tool-call-free
	// "done") while the verify command still fails — but with VerifyContinue it is
	// fed the failure and keeps working, writing the real file on turn 2, which then
	// verifies. The premature stop becomes a genuine Answered instead of Unverified.
	good := "package calc\n\nfunc Eval() int { return 1 }\n"
	turns := [][]llm.ContentPart{
		{llm.Text("All done, the implementation is complete.")},                                  // premature finish
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": good})}, // nudged into real work
		{llm.Text("now actually done")},
	}
	ns := &nativeScript{turns: turns}
	box := sbWith(t, nil)
	// verify is red until calc.go exists, green after the continued work writes it.
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: box, Task: "t", MaxIterations: 10,
		VerifyCmd: "test -f calc.go", VerifyContinue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered — continuation should recover the premature finish", res.Outcome, res.Reason)
	}
	if res.Iterations < 3 {
		t.Errorf("Iterations = %d, want >=3 (it was nudged past the premature finish)", res.Iterations)
	}
	if got := readback(t, box, "calc.go"); got != good {
		t.Errorf("file not written by the continued work: %q", got)
	}
}

func TestRunNativeVerifyContinueStopsAtCap(t *testing.T) {
	// Continuation is bounded: a model that keeps finishing prematurely without ever
	// fixing anything is fed back each time but, at the iteration cap, records the
	// honest Unverified (never an infinite loop).
	turns := [][]llm.ContentPart{{llm.Text("done")}} // every turn is a premature finish.
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, nil), Task: "t", MaxIterations: 4,
		VerifyCmd: "exit 1", VerifyContinue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Unverified {
		t.Fatalf("Outcome = %q (%s), want Unverified at the cap", res.Outcome, res.Reason)
	}
	if res.Iterations != 4 {
		t.Errorf("Iterations = %d, want 4 (continued to the cap, then gave the honest verdict)", res.Iterations)
	}
}

func TestRunNativeChurnNudgeFiresOnce(t *testing.T) {
	// After ChurnNudgeRuns failing test-runs, a one-time rewrite hint is appended to
	// the run observation the model reads — the lever toward whole-file rewrites.
	files := map[string]string{"a.txt": "x\n"}
	turns := [][]llm.ContentPart{
		{failRun("r1")},
		{structuredCall("a", "read_file", map[string]any{"path": "a.txt"})}, // break the run streak (no stagnant kill)
		{failRun("r2")}, // 2nd failure -> nudge
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 20, ChurnNudgeRuns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	nudges := 0
	for _, s := range res.Steps {
		if strings.Contains(s.Observation, "rewrite the whole file") {
			nudges++
		}
	}
	if nudges != 1 {
		t.Errorf("churn nudge appeared %d times, want exactly 1 (fires once at the threshold)", nudges)
	}
}

func TestRunNativeVerifyOnTerminateUpgradesFalseFailure(t *testing.T) {
	// verify-on-terminate: a run that writes correct code but then flails into the
	// iteration cap (the gpt-5-nano r2 pattern — code done, model stuck on a bad
	// side-command) is UPGRADED to Answered because VerifyCmd passes. The verify
	// command is the source of truth; a kill/cap on already-correct code is not a fail.
	good := "package calc\nfunc Eval() int { return 1 }\n"
	turns := [][]llm.ContentPart{
		{structuredCall("w", "write_file", map[string]any{"path": "calc.go", "content": good})},   // code is done here
		{structuredCall("a", "read_file", map[string]any{"path": "calc.go", "from": 1, "to": 1})}, // then flail to the cap
		{structuredCall("b", "read_file", map[string]any{"path": "calc.go", "from": 2, "to": 2})},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, nil), Task: "t", MaxIterations: 3,
		VerifyCmd: "test -f calc.go", // passes once the file exists
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered — verify passed so the cap is not a failure", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Reason, "completed despite hit_cap") {
		t.Errorf("Reason = %q, want it to record the upgraded-from outcome", res.Reason)
	}
}

func TestRunNativeChurnNudgeFiresOnEdits(t *testing.T) {
	// The grok fix: the nudge fires on edit-churn even when the model barely runs the
	// tests (a run-only trigger never fires for it). 3 distinct edits -> nudge.
	turns := [][]llm.ContentPart{
		{structuredCall("e1", "edit_file", map[string]any{"path": "f.txt", "from": 1, "to": 1, "content": "A"})},
		{structuredCall("e2", "edit_file", map[string]any{"path": "f.txt", "from": 1, "to": 1, "content": "B"})},
		{structuredCall("e3", "edit_file", map[string]any{"path": "f.txt", "from": 1, "to": 1, "content": "C"})},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, map[string]string{"f.txt": "x\n"}), Task: "t",
		MaxIterations: 20, ChurnNudgeRuns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	nudges := 0
	for _, s := range res.Steps {
		if strings.Contains(s.Observation, "rewrite the whole file") {
			nudges++
		}
	}
	if nudges != 1 {
		t.Errorf("churn nudge on edits appeared %d times, want exactly 1", nudges)
	}
}

func TestRunNativeWallClockBudget(t *testing.T) {
	// The universal backstop: a run that would otherwise loop is ended as HitDeadline
	// once the wall-clock budget is exceeded, rather than an external timeout (the nano
	// exit-124 case). A 1ns budget trips on the very first between-turn check.
	turns := [][]llm.ContentPart{{structuredCall("c", "read_file", map[string]any{"path": "x"})}}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, nil), Task: "t", MaxIterations: 30, MaxWallClock: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != HitDeadline {
		t.Fatalf("Outcome = %q (%s), want HitDeadline", res.Outcome, res.Reason)
	}
}

func TestRunNativeStagnantObservationKilled(t *testing.T) {
	// The A2 fix: distinct actions (run, read, run, read, run) that each leave the
	// SAME failing `run` result. No exact-repeat (reads break the turn run) and no
	// list_dir spiral — yet the run is stuck. The stagnant-observation detector ends
	// it at the maxStagnant-th identical failure.
	files := map[string]string{"a.txt": "x\n", "b.txt": "y\n"}
	turns := [][]llm.ContentPart{
		{failRun("r1")},
		{structuredCall("a", "read_file", map[string]any{"path": "a.txt"})},
		{failRun("r2")},
		{structuredCall("b", "read_file", map[string]any{"path": "b.txt"})},
		{failRun("r3")}, // third identical failure -> kill.
		{llm.Text("should never reach here")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledStagnant {
		t.Fatalf("Outcome = %q (%s), want KilledStagnant", res.Outcome, res.Reason)
	}
	if res.Iterations != 5 {
		t.Errorf("Iterations = %d, want 5 (killed on the 3rd identical failure)", res.Iterations)
	}
}

func TestRunNativeStagnantThresholdNotTrippedEarly(t *testing.T) {
	// Two identical failures are NOT enough (threshold is maxStagnant=3): the model
	// answers and the run is accepted, proving the detector isn't hair-trigger.
	files := map[string]string{"a.txt": "x\n"}
	turns := [][]llm.ContentPart{
		{failRun("r1")},
		{structuredCall("a", "read_file", map[string]any{"path": "a.txt"})},
		{failRun("r2")},
		{llm.Text("done")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{
		Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered — 2 failures is under the threshold", res.Outcome, res.Reason)
	}
}

func TestRunNativeWriteEnvelopeIsRecoverableObservation(t *testing.T) {
	// The A3 fix end-to-end: a structured write_file whose `content` leaked an
	// apply_patch envelope (R10 gpt-5-nano) is REJECTED as an ERROR observation —
	// the corrupt file is never written — and the model recovers with a clean write.
	good := "package main\n\nfunc main() {}\n"
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "calc.go", "content": "*** Begin Patch\n*** Add File: calc.go\n" + good})},
		{structuredCall("c2", "write_file", map[string]any{"path": "calc.go", "content": good})},
		{llm.Text("written cleanly")},
	}
	res, _, sb := runNative(t, nil, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if !strings.Contains(res.Steps[0].Observation, "ERROR") || !strings.Contains(res.Steps[0].Observation, "patch/diff/fence") {
		t.Errorf("envelope write observation = %q, want an ERROR with a patch-wrapper recovery", res.Steps[0].Observation)
	}
	if got := readback(t, sb, "calc.go"); got != good {
		t.Errorf("final file = %q, want the clean second write (the envelope must never land)", got)
	}
}

func TestRunNativeRepeatDetectorOnStructuredArgs(t *testing.T) {
	// The no-progress detector must key on the structured args: the SAME typed call
	// repeated maxRepeats+1 times trips KilledRepeat, exactly as a repeated text arg
	// would. (list_dir is exempt from the spiral detector only; repeat still applies.)
	call := structuredCall("c", "read_file", map[string]any{"path": "f.txt"})
	turns := [][]llm.ContentPart{{call}, {call}, {call}, {call}}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"f.txt": "x\n"}), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledRepeat {
		t.Errorf("Outcome = %q, want KilledRepeat", res.Outcome)
	}
}
