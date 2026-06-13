package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
// stands in for a native provider. A turn that carries a ReasoningPart also
// reports ReasoningTokens in its usage — like every real reasoner measured
// (glm-5, deepseek) — unless nonceTrace emulates the gemini-via-OpenRouter
// pathology: an encrypted thought-signature that differs every call while usage
// reports zero reasoning tokens (DUET-DOGFOOD N3).
type nativeScript struct {
	turns      [][]llm.ContentPart
	calls      []llm.Request
	n          int
	nonceTrace bool
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
	usage := llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	for _, p := range parts {
		if _, ok := p.(llm.ToolCallPart); ok {
			fr = llm.FinishToolUse
		}
		if _, ok := p.(llm.ReasoningPart); ok && !s.nonceTrace {
			usage.ReasoningTokens = 3
		}
	}
	return &llm.Response{Content: parts, FinishReason: fr, Usage: usage}, nil
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

// A no-tool turn with empty prose (and no FinishTool configured) is the model
// going silent, not a finished task. Without the guard it was recorded as
// Answered/exit-0, so an empty final answer read as a clean pass — flag it
// Unverified instead.
func TestRunNativeEmptyAnswerIsUnverified(t *testing.T) {
	res, _, _ := runNative(t, nil, [][]llm.ContentPart{{llm.Text("")}})
	if res.Outcome != Unverified {
		t.Fatalf("Outcome = %q (%s), want Unverified for an empty final answer", res.Outcome, res.Reason)
	}
	if res.Reason == "" {
		t.Errorf("expected a reason explaining the empty-answer rejection")
	}
}

// sayTool is a minimal first-class finish tool, mirroring duet.SayTool (the agent
// package can't import duet). It is wired as Config.FinishTool in the tests below.
func sayTool() Tool {
	return Tool{
		Name:    "say",
		Schema:  json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		RunJSON: func(_ context.Context, _ json.RawMessage) (string, error) { return "sent", nil },
	}
}

func TestRunNativeFinishToolNearCapNudgesSay(t *testing.T) {
	// DUET-DOGFOOD F2: a finish-tool agent that works right up to the cap loses
	// its message (validation duet 2026-06-12, turn 7: file written and tested at
	// i14–16, `say` never made). Within finishToolNudgeWindow of the cap the loop
	// must inject a once-only reminder to call the finish tool, leaving a turn to
	// act — and a model that heeds it ends Answered.
	sb := sbWith(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"})
	tools := DefaultTools(sb, 0)
	tools["say"] = sayTool()
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{structuredCall("c1", "read_file", map[string]any{"path": "a.txt"})},
		{structuredCall("c2", "read_file", map[string]any{"path": "b.txt"})},
		{structuredCall("c3", "read_file", map[string]any{"path": "c.txt"})},
		{structuredCall("c4", "say", map[string]any{"message": "done — wrapping up"})},
	}}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", Tools: tools, FinishTool: "say", MaxIterations: 4})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered", res.Outcome, res.Reason)
	}
	// The reminder must have been in the conversation BEFORE the final model call
	// (maxIter=4, window=2 → it fires after turn 2's results land), and only once.
	nudges := 0
	for _, m := range ns.calls[len(ns.calls)-1].Messages {
		if m.Role == llm.RoleUser && strings.Contains(m.Text(), "budget is nearly spent") {
			nudges++
		}
	}
	if nudges != 1 {
		t.Errorf("finish-tool nudge appeared %d times in the final request, want exactly 1", nudges)
	}
}

func TestRunNativeFinishToolTerminatesAsAnswered(t *testing.T) {
	// F1 fix: calling the designated finish tool ends the turn cleanly as Answered,
	// with its message as the answer — no need to reply tool-call-free in prose.
	sb := sbWith(t, nil)
	tools := DefaultTools(sb, 0)
	tools["say"] = sayTool()
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{structuredCall("c1", "say", map[string]any{"message": "hey partner, built the thing"})},
	}}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", Tools: tools, FinishTool: "say"})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered", res.Outcome, res.Reason)
	}
	if res.Answer != "hey partner, built the thing" {
		t.Errorf("Answer = %q", res.Answer)
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (finished on the first turn)", res.Iterations)
	}
}

func TestRunNativeFinishToolRunsSideEffectsFirst(t *testing.T) {
	// A turn may carry a final write/build alongside `say`; the side effect must
	// land before the turn terminates (N2: act-then-finish in one move).
	sb := sbWith(t, nil)
	tools := DefaultTools(sb, 0)
	tools["say"] = sayTool()
	ns := &nativeScript{turns: [][]llm.ContentPart{{
		structuredCall("c1", "write_file", map[string]any{"path": "out.txt", "content": "data"}),
		structuredCall("c2", "say", map[string]any{"message": "dropped out.txt"}),
	}}}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", Tools: tools, FinishTool: "say"})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Answered || res.Answer != "dropped out.txt" {
		t.Fatalf("Outcome=%q Answer=%q", res.Outcome, res.Answer)
	}
	if got := readback(t, sb, "out.txt"); got != "data" {
		t.Errorf("side-effect write did not land before finish: %q", got)
	}
}

func TestRunNativeSalvagesProseOnHitCap(t *testing.T) {
	// N1: a run that never finishes (hit_cap) but narrated prose alongside its tool
	// calls should surface that prose as the answer, not empty silence — so a caller
	// relaying it (duet) isn't handed nothing.
	// Distinct turns each iteration so the tight-loop detector doesn't kill it first
	// — we want it to genuinely run out the cap with prose left on the table.
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("reading the spec"), structuredCall("c1", "read_file", map[string]any{"path": "a.txt"})},
		{llm.Text("checking the impl"), structuredCall("c2", "read_file", map[string]any{"path": "b.txt"})},
		{llm.Text("still wiring it up, one sec"), structuredCall("c3", "read_file", map[string]any{"path": "c.txt"})},
	}}
	sb := sbWith(t, map[string]string{"a.txt": "1\n", "b.txt": "2\n", "c.txt": "3\n"})
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", MaxIterations: 3})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != HitCap {
		t.Fatalf("Outcome = %q, want HitCap", res.Outcome)
	}
	if res.Answer != "still wiring it up, one sec" {
		t.Errorf("salvaged Answer = %q, want the last prose", res.Answer)
	}
}

func TestRunNativeEmptySilentFinishNudgedToFinishTool(t *testing.T) {
	// With a FinishTool configured, an empty no-tool-call turn (the model going
	// silent) must NOT be accepted as a clean answer: the loop nudges and the model
	// finishes properly via the finish tool on the next turn.
	sb := sbWith(t, nil)
	tools := DefaultTools(sb, 0)
	tools["say"] = sayTool()
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.Text("")}, // empty silent finish — should be rejected
		{structuredCall("c1", "say", map[string]any{"message": "ok here's my actual message"})},
	}}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", Tools: tools, FinishTool: "say"})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Answered || res.Answer != "ok here's my actual message" {
		t.Fatalf("Outcome=%q Answer=%q — empty finish should have been nudged to say", res.Outcome, res.Answer)
	}
}

func TestRunNativeReasoningOnlyEmptyTurnNotNudgedToFinish(t *testing.T) {
	// A reasoning model (deepseek-v4-flash) routinely emits a THINK-ONLY turn —
	// reasoning advanced, no text, no tool call — then acts on the next turn. That
	// is mid-thought, NOT a finish attempt: the loop must NOT inject the say-nudge
	// ("...use the say tool to finish your turn"), which mis-instructs a model that
	// isn't done. It should carry the reasoning forward and continue silently.
	sb := sbWith(t, nil)
	tools := DefaultTools(sb, 0)
	tools["say"] = sayTool()
	ns := &nativeScript{turns: [][]llm.ContentPart{
		{llm.ReasoningPart{Raw: json.RawMessage(`"planning my next move"`)}}, // think-only — must NOT be nudged
		{structuredCall("c1", "say", map[string]any{"message": "done thinking, here it is"})},
	}}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", Tools: tools, FinishTool: "say"})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Answered || res.Answer != "done thinking, here it is" {
		t.Fatalf("Outcome=%q Answer=%q — think-only turn should continue silently to the say", res.Outcome, res.Answer)
	}
	// No request may carry the say-nudge: a reasoning-advanced empty turn is not
	// silence, so the model must never have been told to wrap up.
	for _, req := range ns.calls {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Text(), "without saying anything") {
				t.Fatalf("think-only turn was nudged to finish — found say-nudge in a request")
			}
		}
	}
	// The think-only turn's reasoning must be replayed (the thought trace is carried
	// forward so the model can build on it), so the final request includes it.
	last := ns.calls[len(ns.calls)-1]
	var sawReasoning bool
	for _, m := range last.Messages {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			if _, ok := p.(llm.ReasoningPart); ok {
				sawReasoning = true
			}
		}
	}
	if !sawReasoning {
		t.Errorf("reasoning from the think-only turn was not carried forward to the next request")
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

func TestRunNativeWriteAppendBuildsFileInPieces(t *testing.T) {
	// DUET-DOGFOOD F7 recovery path: a large file built in pieces — a plain write
	// for the head, then append:true for each next chunk. append on a MISSING
	// file creates it (`>>` semantics), so the very first call may also append.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "write_file", map[string]any{"path": "big.go", "content": "package main\n"})},
		{structuredCall("c2", "write_file", map[string]any{"path": "big.go", "content": "func main() {}\n", "append": true})},
		{structuredCall("c3", "write_file", map[string]any{"path": "fresh.txt", "content": "made by append\n", "append": true})},
		{llm.Text("done")},
	}
	res, _, sb := runNative(t, nil, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if got, want := readback(t, sb, "big.go"), "package main\nfunc main() {}\n"; got != want {
		t.Errorf("assembled file = %q, want %q", got, want)
	}
	if !strings.Contains(res.Steps[1].Observation, "appended") {
		t.Errorf("append observation = %q, want an 'appended' confirmation", res.Steps[1].Observation)
	}
	if got, want := readback(t, sb, "fresh.txt"), "made by append\n"; got != want {
		t.Errorf("append-to-missing = %q, want %q (>> semantics: create)", got, want)
	}
}

func TestRunNativeTruncatedWriteArgsTeachAppendRecovery(t *testing.T) {
	// DUET-DOGFOOD F7: deepseek-v4-flash truncates oversized tool-call args
	// mid-string, and the model's instinct is to retry the same call to the cap
	// (9 and 13 identical retries in the 2026-06-12 validation run). The error
	// observation must name the truncation AND point at the chunked-append
	// recovery — not read as a generic parse failure.
	truncated := llm.ToolCallPart{ID: "c1", Name: "write_file",
		Args: json.RawMessage(`{"path": "big.go", "content": "package main\nfunc ma`)} // cut mid-string
	turns := [][]llm.ContentPart{
		{truncated},
		{llm.Text("ok, I'll chunk it")},
	}
	res, _, _ := runNative(t, nil, turns)
	obs := res.Steps[0].Observation
	for _, want := range []string{"TRUNCATED", "append", "Do NOT retry"} {
		if !strings.Contains(obs, want) {
			t.Errorf("truncation observation missing %q: %q", want, obs)
		}
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
		{structuredCall("c1", "edit_file", map[string]any{"path": "f.txt", "old": "OLD", "new": repl})},
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
	// {path, old, new} replaces the unique occurrence of `old`.
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "edit_file", map[string]any{"path": "f.txt", "old": "b", "new": "BB"})},
		{llm.Text("done")},
	}
	res, _, sb := runNative(t, map[string]string{"f.txt": "a\nb\nc\n"}, turns)
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if got, want := readback(t, sb, "f.txt"), "a\nBB\nc\n"; got != want {
		t.Errorf("after replace = %q, want %q", got, want)
	}
	// The recorded arg carries the typed anchor fields, not a positional line range.
	if !strings.Contains(res.Steps[0].Arg, "\"old\"") || strings.Contains(res.Steps[0].Arg, ":2-2") {
		t.Errorf("Step.Arg should record typed old/new fields, not a line range: %q", res.Steps[0].Arg)
	}
}

func TestRunNativeEditFileStructuredDelete(t *testing.T) {
	// new == "" deletes the matched text (the anchor includes the line's newline).
	turns := [][]llm.ContentPart{
		{structuredCall("c1", "edit_file", map[string]any{"path": "f.txt", "old": "b\n", "new": ""})},
		{llm.Text("done")},
	}
	_, _, sb := runNative(t, map[string]string{"f.txt": "a\nb\nc\n"}, turns)
	if got, want := readback(t, sb, "f.txt"), "a\nc\n"; got != want {
		t.Errorf("after delete = %q, want %q", got, want)
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

func TestRunNativeNavSpiralWindowRelaxes(t *testing.T) {
	// NavSpiralWindow raises the spiral threshold for an OBSERVE-only caller (the
	// council code critic): the same four list_dir-only turns that trip the default
	// detector (above) survive when the window is 8, then the run answers. The
	// opt-in relaxation must not weaken the default for everyone else — the test
	// above proves the default still fires.
	files := map[string]string{"a/x": "1", "b/x": "1", "c/x": "1", "d/x": "1"}
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("2", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("3", "list_dir", map[string]any{"path": "c"})},
		{structuredCall("4", "list_dir", map[string]any{"path": "d"})},
		{llm.Text("[]")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 10, NavSpiralWindow: 8})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered — NavSpiralWindow=8 should not kill 4 list_dir turns", res.Outcome, res.Reason)
	}
}

func TestRunNativeAnswerNudgeForcesAnswer(t *testing.T) {
	// DOGFOOD slice 4: an observe-only critic that calls a tool EVERY turn never
	// emits a no-tool-call answer turn and hits the cap with no output. AnswerNudgeWindow
	// injects a near-cap "stop and answer" hint; this script reads on every turn until
	// the nudge arrives, then answers — proving the nudge converts a would-be hit_cap
	// into Answered. With the window OFF (default) the same script hits the cap.
	// Distinct files each turn so the tight-repeat detector (same verb+arg) doesn't
	// fire before the nudge — this models a critic genuinely reading new files.
	files := map[string]string{"f0": "a", "f1": "a", "f2": "a", "f3": "a", "f4": "a", "f5": "a"}
	mkTurns := func() [][]llm.ContentPart {
		turns := [][]llm.ContentPart{}
		for i := 0; i < 6; i++ {
			turns = append(turns, []llm.ContentPart{structuredCall("r", "read_file", map[string]any{"path": fmt.Sprintf("f%d", i)})})
		}
		return append(turns, []llm.ContentPart{llm.Text("[]")})
	}
	nudged := func(reqs []llm.Request) bool {
		for _, req := range reqs {
			for _, m := range req.Messages {
				if m.Role == llm.RoleUser && strings.Contains(m.Text(), "STOP exploring now") {
					return true
				}
			}
		}
		return false
	}

	// Observe-only toolset (no run/write/edit): the nudge is SAFE and fires.
	ro := DefaultTools(sbWith(t, files), time.Second)
	delete(ro, "run")
	delete(ro, "write_file")
	delete(ro, "edit_file")
	ns := &nativeScript{turns: mkTurns()}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Tools: ro, Task: "t", MaxIterations: 8, AnswerNudgeWindow: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered with AnswerNudgeWindow set", res.Outcome, res.Reason)
	}
	if !nudged(ns.calls) {
		t.Error("observe-only + AnswerNudgeWindow: the answer-forcing hint was never injected")
	}

	// O2 safety gate: a FULL toolset (has run/write/edit) with no verify gate must NOT
	// fire the nudge — else a coding caller's premature "done" would be accepted unchecked.
	ns2 := &nativeScript{turns: mkTurns()}
	_, err = RunNative(context.Background(), Config{Model: ns2, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 8, AnswerNudgeWindow: 3})
	if err != nil {
		t.Fatal(err)
	}
	if nudged(ns2.calls) {
		t.Error("full toolset without a verify gate must NOT receive the answer nudge (O2 safety)")
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

func TestRunNativeSearchSpiralAcrossTurns(t *testing.T) {
	// HP-2 generalization: the spiral detector keys on the discovery CLASS, not on
	// list_dir alone, so search-churn — searching for a NEW pattern every turn,
	// never reading a result — is the same wandering and must end as KilledSpiral.
	// The detector runs on the model's CALLS before dispatch, so it fires regardless
	// of whether ripgrep finds anything in the test sandbox.
	files := map[string]string{"a.go": "package a\n"}
	turns := [][]llm.ContentPart{
		{structuredCall("1", "search", map[string]any{"pattern": "alpha"})},
		{structuredCall("2", "search", map[string]any{"pattern": "beta"})},
		{structuredCall("3", "search", map[string]any{"pattern": "gamma"})},
		{structuredCall("4", "search", map[string]any{"pattern": "delta"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Errorf("Outcome = %q (%s), want KilledSpiral — search-churn is a discovery spiral", res.Outcome, res.Reason)
	}
}

func TestRunNativeMixedDiscoverySpiral(t *testing.T) {
	// Alternating list_dir and search — no single verb repeats, so the old
	// list_dir-only check never fired — is still pure discovery: pointers gathered,
	// none followed. Keying on the discovery class catches it.
	files := map[string]string{"a/x": "1", "b/x": "1"}
	turns := [][]llm.ContentPart{
		{structuredCall("1", "list_dir", map[string]any{"path": "a"})},
		{structuredCall("2", "search", map[string]any{"pattern": "alpha"})},
		{structuredCall("3", "list_dir", map[string]any{"path": "b"})},
		{structuredCall("4", "search", map[string]any{"pattern": "beta"})},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Errorf("Outcome = %q (%s), want KilledSpiral — mixed list_dir/search wandering is a discovery spiral", res.Outcome, res.Reason)
	}
}

func TestRunNativeSearchThenReadResetsSpiral(t *testing.T) {
	// The false-positive guard: search-then-read reconnaissance — search for a
	// symbol, then READ the file it points to — is productive and must NOT be killed.
	// A read_file turn is not discovery-only, so it breaks the spiral run; this
	// search/read/search/read pattern never reaches the window and answers cleanly.
	// This is what makes including search safe (HP2-TEMPLATE-COLLAPSE.md).
	files := map[string]string{"a.go": "package a\n", "b.go": "package b\n"}
	turns := [][]llm.ContentPart{
		{structuredCall("1", "search", map[string]any{"pattern": "package"})},
		{structuredCall("2", "read_file", map[string]any{"path": "a.go"})},
		{structuredCall("3", "search", map[string]any{"pattern": "import"})},
		{structuredCall("4", "read_file", map[string]any{"path": "b.go"})},
		{structuredCall("5", "search", map[string]any{"pattern": "func"})},
		{llm.Text("found what I needed")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered — search-then-read recon must not trip the spiral", res.Outcome, res.Reason)
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
		{structuredCall("e1", "edit_file", map[string]any{"path": "f.txt", "old": "x", "new": "A"})},
		{structuredCall("e2", "edit_file", map[string]any{"path": "f.txt", "old": "A", "new": "B"})},
		{structuredCall("e3", "edit_file", map[string]any{"path": "f.txt", "old": "B", "new": "C"})},
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

func TestRunNativeReasoningAdvanceEscapesRepeatDetector(t *testing.T) {
	// The Gemini fix: a THINKING model re-issues the same visible action across turns
	// while its (opaque) reasoning advances — that's hidden progress, not a tight
	// loop, so it must NOT be killed at maxRepeats. Same read_file 3×, each with a
	// DIFFERENT reasoning trace, then the model answers: the run reaches Answered
	// instead of KilledRepeat. (Contrast TestRunNativeRepeatDetectorOnStructuredArgs,
	// where the identical call carries no reasoning and IS killed.)
	call := structuredCall("c", "read_file", map[string]any{"path": "f.txt"})
	turns := [][]llm.ContentPart{
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"a"}]`)}, call},
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"b"}]`)}, call},
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"c"}]`)}, call},
		{llm.Text("the bug is in percent.go")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"f.txt": "x\n"}), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered — advancing reasoning is progress, not a tight loop", res.Outcome, res.Reason)
	}
}

func TestRunNativeFrozenReasoningStillKilled(t *testing.T) {
	// The other half: a reasoning model whose action AND reasoning both froze (the
	// identical block byte-for-byte every turn) is genuinely stalled and must still
	// trip KilledRepeat — the leniency is only for reasoning that actually moves.
	frozen := llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"same"}]`)}
	call := structuredCall("c", "read_file", map[string]any{"path": "f.txt"})
	turns := [][]llm.ContentPart{{frozen, call}, {frozen, call}, {frozen, call}, {frozen, call}}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"f.txt": "x\n"}), Task: "t", MaxIterations: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledRepeat {
		t.Errorf("Outcome = %q, want KilledRepeat — frozen reasoning is a real stall", res.Outcome)
	}
}

// slowNative is a tool-calling provider that sleeps before replying, so a test can
// assert the native loop actually MEASURES model latency (regression for the bug
// where RunNative left ModelMs/ToolMs unset, zeroing the eval latency column on the
// default path).
type slowNative struct {
	turns [][]llm.ContentPart
	delay time.Duration
	n     int
}

func (s *slowNative) Name() string                   { return "slow-native" }
func (s *slowNative) Capabilities() llm.Capabilities { return llm.Capabilities{Tools: true} }
func (s *slowNative) Stream(context.Context, llm.Request) iter.Seq2[llm.Chunk, error] {
	return llm.UnsupportedStream("slow-native")
}
func (s *slowNative) Generate(_ context.Context, _ llm.Request) (*llm.Response, error) {
	time.Sleep(s.delay)
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

func TestRunNativeRecordsStepTiming(t *testing.T) {
	// O1 regression: native runs (the default eval path) must record per-turn model
	// latency and per-tool latency, not leave them zero.
	sp := &slowNative{delay: 8 * time.Millisecond, turns: [][]llm.ContentPart{
		{structuredCall("c1", "read_file", map[string]any{"path": "go.mod"})},
		{llm.Text("done")},
	}}
	res, err := RunNative(context.Background(), Config{Model: sp, Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}), Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) < 2 {
		t.Fatalf("want >=2 steps, got %d", len(res.Steps))
	}
	if res.Steps[0].ModelMs <= 0 {
		t.Errorf("tool-turn step ModelMs = %d, want >0 (native model latency not measured)", res.Steps[0].ModelMs)
	}
	// The answer turn dispatches no tool, so its ToolMs is 0.
	last := res.Steps[len(res.Steps)-1]
	if last.Verb == "answer" && last.ToolMs != 0 {
		t.Errorf("answer-turn ToolMs = %d, want 0", last.ToolMs)
	}
	// Trial-style total latency is non-zero (what the eval column sums).
	var total int64
	for _, s := range res.Steps {
		total += s.ModelMs + s.ToolMs
	}
	if total <= 0 {
		t.Errorf("summed step latency = %d, want >0", total)
	}
}

func TestRunNativeRecordsReasoningAdvanced(t *testing.T) {
	// O2 regression: native steps must carry ReasoningAdvanced so a transcript shows
	// when the lenient repeat threshold was in play. Two turns with DIFFERENT reasoning
	// traces -> the second turn's step is flagged advanced.
	turns := [][]llm.ContentPart{
		{llm.ReasoningPart{Raw: []byte(`"t1"`)}, structuredCall("c1", "list_dir", map[string]any{"path": "."})},
		{llm.ReasoningPart{Raw: []byte(`"t2"`)}, structuredCall("c2", "read_file", map[string]any{"path": "go.mod"})},
		{llm.Text("done")},
	}
	res, _, _ := runNative(t, map[string]string{"go.mod": "module x\n"}, turns)
	if len(res.Steps) < 2 {
		t.Fatalf("want >=2 steps, got %d", len(res.Steps))
	}
	// Turn 2's step (read_file) had a reasoning trace differing from turn 1 -> advanced.
	var readStep *Step
	for i := range res.Steps {
		if res.Steps[i].Verb == "read_file" {
			readStep = &res.Steps[i]
		}
	}
	if readStep == nil || !readStep.ReasoningAdvanced {
		t.Errorf("read_file step ReasoningAdvanced not set: %+v", readStep)
	}
}

func TestRunNativeDiscoveryOrientationBurstWithReasoningSurvives(t *testing.T) {
	// The measured glm-5 false-kill (SWE-bench stride-30, 2026-06-12): the model
	// opens with a grounded descend-the-tree orientation burst — list_dir . →
	// pkg → pkg/sub → a targeted search — reasoning visibly advancing every turn,
	// and was executed at exactly the strict window on instances other models
	// solved 2/2. Five discovery turns with a MOVING trace must survive the
	// strict window (4), reach the read, and answer.
	files := map[string]string{"pkg/sub/f.txt": "x\n"}
	turns := [][]llm.ContentPart{
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"r1"}]`)}, structuredCall("1", "list_dir", map[string]any{"path": "."})},
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"r2"}]`)}, structuredCall("2", "list_dir", map[string]any{"path": "pkg"})},
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"r3"}]`)}, structuredCall("3", "list_dir", map[string]any{"path": "pkg/sub"})},
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"r4"}]`)}, structuredCall("4", "search", map[string]any{"pattern": "alpha"})},
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"r5"}]`)}, structuredCall("5", "search", map[string]any{"pattern": "beta"})},
		{llm.ReasoningPart{Raw: json.RawMessage(`[{"data":"r6"}]`)}, structuredCall("6", "read_file", map[string]any{"path": "pkg/sub/f.txt"})},
		{llm.Text("found it")},
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, files), Task: "t", MaxIterations: 12})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Errorf("Outcome = %q (%s), want Answered — an orientation burst with advancing reasoning is not a spiral", res.Outcome, res.Reason)
	}
}

func TestRunNativeDiscoverySpiralWithReasoningStillBounded(t *testing.T) {
	// The leniency is a doubled window, not immunity: a reasoning model that does
	// NOTHING but discovery — trace moving, pointers gathered, none ever followed —
	// must still die at 2× the window (8), well before the iteration cap.
	turns := make([][]llm.ContentPart, 0, 9)
	for i := 0; i < 9; i++ {
		turns = append(turns, []llm.ContentPart{
			llm.ReasoningPart{Raw: json.RawMessage(fmt.Sprintf(`[{"data":"r%d"}]`, i))},
			structuredCall(fmt.Sprintf("c%d", i), "search", map[string]any{"pattern": fmt.Sprintf("p%d", i)}),
		})
	}
	ns := &nativeScript{turns: turns}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"a.go": "package a\n"}), Task: "t", MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledSpiral {
		t.Errorf("Outcome = %q (%s), want KilledSpiral at the doubled window — reasoning leniency must stay bounded", res.Outcome, res.Reason)
	}
	if n := len(res.Steps); n != 8 {
		t.Errorf("killed after %d steps, want 8 (2× the strict window)", n)
	}
}

func TestRunNativeZeroTokenMovingTraceKeepsLenientCeiling(t *testing.T) {
	// A DECIDED behavior, not an accident (DUET-DOGFOOD N3, tried and REVERTED
	// 2026-06-12): a reasoning trace that moves every call while usage reports
	// ZERO reasoning tokens still buys the lenient repeat ceiling. Gemini via
	// OpenRouter is exactly this shape — an encrypted thought-signature with no
	// token count — and gating leniency on ReasoningTokens > 0 false-killed its
	// digest-re-read pattern (trace eval 5/5 → 0/5, eval/runs/n3gate-trace-gemini).
	// The kill stays BOUNDED at maxReasoningRepeats, well before the cap.
	turns := make([][]llm.ContentPart, 0, 12)
	for i := 0; i < 12; i++ {
		turns = append(turns, []llm.ContentPart{
			llm.ReasoningPart{Raw: json.RawMessage(fmt.Sprintf(`[{"signature":"nonce-%d"}]`, i))},
			structuredCall(fmt.Sprintf("c%d", i), "read_file", map[string]any{"path": "a.go"}),
		})
	}
	ns := &nativeScript{turns: turns, nonceTrace: true}
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sbWith(t, map[string]string{"a.go": "package a\n"}), Task: "t", MaxIterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != KilledRepeat {
		t.Fatalf("Outcome = %q (%s), want KilledRepeat at the LENIENT ceiling (bounded, not immune)", res.Outcome, res.Reason)
	}
	if n := res.Iterations; n <= maxRepeats+1 {
		t.Errorf("killed after %d iterations — the strict ceiling fired; a moving zero-token trace must get the lenient one (the gemini digest-re-read false-kill)", n)
	}
	for _, s := range res.Steps[1:] { // turn 1 has no prior trace to compare.
		if !s.ReasoningAdvanced {
			t.Errorf("step %d ReasoningAdvanced = false — a moving trace counts as advancing regardless of token count", s.Iter)
		}
	}
}
