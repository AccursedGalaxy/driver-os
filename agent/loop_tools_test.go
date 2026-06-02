package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
