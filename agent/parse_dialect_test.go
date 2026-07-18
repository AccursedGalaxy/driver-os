package agent

import "testing"

// Dialect-repair suite (rung-0 trim sweep 2026-07-17/18, freeze v1): the 43
// killed_repeat attempts were ~80% chat-template DIALECT, not decisions —
// the sampled local model emits the RIGHT action serialized in a grammar the
// text loop rejects, the error feedback does not pull it back, and the
// loop-kill fires (docs/findings/RUNG0-REPEAT-KILL-ANATOMY.md in lab; ~38 of
// the 89 failed attempts are this class). Every accepted form below is a
// verbatim reply mined from eval/runs/rung0-trim traces; the negatives pin
// fail-closed behavior for everything that is NOT deterministically
// normalizable (unknown verbs, wrapper-only replies, degenerate tokens, bare
// prose/shell) and for legitimate args that merely contain angle brackets.
//
// parseAction stays a pure function; repairs are silent accepts, same as the
// d31f789 tag-wrapped form.

func dialectTools() map[string]Tool {
	return map[string]Tool{
		"read_file": {Name: "read_file"},
		"edit_file": {Name: "edit_file"},
		"list_dir":  {Name: "list_dir"},
		"search":    {Name: "search"},
		"run":       {Name: "run"},
	}
}

type dialectCase struct {
	name  string
	reply string
	verb  string
	arg   string
}

func runDialectCases(t *testing.T, cases []dialectCase) {
	t.Helper()
	for _, c := range cases {
		v, arg := parseAction(c.reply, dialectTools())
		if v != c.verb || arg != c.arg {
			t.Errorf("%s: parseAction(%q) = (%q, %q), want (%q, %q)", c.name, c.reply, v, arg, c.verb, c.arg)
		}
	}
}

// Class 1 — arg-side wrapper tokens (26/43 kills): the verb parses but the
// argument is poisoned with <argument>/<argument_value> wrapper junk. The
// exact wrapper tag tokens are stripped from the arg — nothing else.
func TestParseActionArgWrapperTokensStripped(t *testing.T) {
	runDialectCases(t, []dialectCase{
		{"argument-token", "list_dir <argument> .", "list_dir", "."},
		{"argument-token-path", "read_file <argument> agent/review.go", "read_file", "agent/review.go"},
		{"trailing-close-fused", "list_dir <argument> agent</argument_value>", "list_dir", "agent"},
		{"value-wrapped", "read_file <argument> <argument_value>agent/gate.go</argument_value>", "read_file", "agent/gate.go"},
		{"tool_call-prefix-line", "<tool_call>list_dir <argument> .", "list_dir", "."},
		// arg content with a real `>` redirect survives; only exact wrapper tags go.
		{"redirect-preserved", `<tool_call>run <argument> find / -name "gate.go" 2>/dev/null`, "run", `find / -name "gate.go" 2>/dev/null`},
	})
}

// Class 2 — argument on the FOLLOWING line inside a wrapper block. Verbatim
// from run core-0f45d79ccf02-a1-r0 iter 1 (burned the whole attempt on
// `no such file: "<argument>"`).
func TestParseActionMultiLineWrappedArg(t *testing.T) {
	runDialectCases(t, []dialectCase{
		{"argument-block", "<tool_call>\n<tool_call>\nread_file <argument>\nagent/tools.go\n</argument>\n</argument>", "read_file", "agent/tools.go"},
		{"parameter-eq-block", "<tool_call>\n<parameter=read_file>\nagent/loop_tools.go\n</parameter>\n</function>", "read_file", "agent/loop_tools.go"},
		{"parameter-eq-nested-arg", "<tool_call>\n<parameter=run>\n<parameter=argument>\ngrep -n \"RestoreTree\" vcs/vcs.go\n</parameter>\n</parameter>\n</function>\n</tool_call>", "run", `grep -n "RestoreTree" vcs/vcs.go`},
	})
}

// Class 3 — <parameter=verb> / <parameter name="verb"> forms with the
// argument on the same line or in the tag body.
func TestParseActionParameterForms(t *testing.T) {
	runDialectCases(t, []dialectCase{
		{"parameter-eq-sameline", "<parameter=list_dir> provider", "list_dir", "provider"},
		{"parameter-name-attr", `<tool_call>` + "\n" + `<parameter name="read_file">agent/loop_tools.go:60-70</parameter>` + "\n" + `</parameter>`, "read_file", "agent/loop_tools.go:60-70"},
	})
}

// Class 4 — verb and argument INSIDE one angle-bracket token: closed,
// self-closed-ish, or unclosed.
func TestParseActionAngleVerbWithInlineArg(t *testing.T) {
	runDialectCases(t, []dialectCase{
		{"closed", "<list_dir .>", "list_dir", "."},
		{"closed-arg", "<list_dir agent>", "list_dir", "agent"},
		{"close-tag-fused", "<list_dir agent</list_dir>", "list_dir", "agent"},
		{"run-close-tag", "<run ls -la agent/</run>", "run", "ls -la agent/"},
		{"trailing-gt-range", "<tool_call>\n<read_file provider/openaicompat/openaicompat.go:151-300>", "read_file", "provider/openaicompat/openaicompat.go:151-300"},
		{"unclosed", "<read_file provider/openaicompat/openaicompat.go", "read_file", "provider/openaicompat/openaicompat.go"},
		{"tag-then-arg-nospace", "<tool_call>\n<read_file>agent/loop_tools.go", "read_file", "agent/loop_tools.go"},
	})
}

// Class 5 — single-character prefixes fused onto a known verb: backslash
// (\verb) and the <tool_call>_verb fusion.
func TestParseActionPrefixFusions(t *testing.T) {
	runDialectCases(t, []dialectCase{
		{"backslash-list", `\list_dir agent`, "list_dir", "agent"},
		{"backslash-run", `\run pwd`, "run", "pwd"},
		{"backslash-search", `\search runFingerprint agent/tools.go`, "search", "runFingerprint agent/tools.go"},
		{"backslash-answer", `\answer The loop is fixed; go test ./agent passes.`, "answer", "The loop is fixed; go test ./agent passes."},
		{"tool_call-underscore", "<tool_call>_read_file provider/openaicompat/openaicompat.go", "read_file", "provider/openaicompat/openaicompat.go"},
	})
}

// Class 6 — JSON action object (measured verbatim, n=3). Only the exact
// measured shape: {"action": <known verb>, "argument": <string>}.
func TestParseActionJSONForm(t *testing.T) {
	runDialectCases(t, []dialectCase{
		{"json-object", `{"action":"list_dir","argument":"agent"}`, "list_dir", "agent"},
	})
}

// Fail-closed: everything below stays UNRECOGNIZED (verb == ""). Each is a
// real mined reply that is not deterministically normalizable.
func TestParseActionDialectFailClosed(t *testing.T) {
	for _, reply := range []string{
		"<tool_call>\n<tool_call>\n<tool_call>", // wrapper-only degeneration (242 steps — generation-side)
		"",                                      // empty reply
		"<|mask_start|>mask_end|>",              // degenerate special tokens
		"<|mask_start|><think>",                 // ditto
		"<go_doc -all llm",                      // unknown verb in angle form
		"<tool_call>_dir provider/openaicompat", // fused-and-truncated verb — `dir` is not a tool
		"<",                                     // lone bracket
		"read_file>",                            // bare `verb>` with no argument anywhere
		"<read_file>",                           // verb tag, no argument anywhere (pins d31f789 behavior)
		`go test ./agent -v -run "Spiral"`,      // bare shell line, no verb — never guess `run`
		"All tests pass. The two changes made:", // prose
		"```bash",                               // fence line
		`{"action":"go_doc","argument":"llm"}`,  // JSON with unknown verb
		`{"argument":"agent"}`,                  // JSON without an action
	} {
		if v, _ := parseAction(reply, dialectTools()); v != "" {
			t.Errorf("reply %q parsed as %q, want unrecognized (fail-closed)", reply, v)
		}
	}
}

// Legitimate args that merely contain angle brackets must pass through
// untouched — the repair strips only exact wrapper-tag tokens.
func TestParseActionLegitimateArgsUntouched(t *testing.T) {
	runDialectCases(t, []dialectCase{
		{"heredoc", `run cat > /tmp/gen.py << 'PYEOF'`, "run", `cat > /tmp/gen.py << 'PYEOF'`},
		{"chan-search", `search <-chan`, "search", `<-chan`},
		{"redirect", `run ls -la agent/ 2>&1`, "run", `ls -la agent/ 2>&1`},
		// A genuinely bare plain verb keeps its documented empty-arg meaning
		// (list_dir "" lists the root); forward pickup fires only on wrapper
		// junk, never on a bare verb followed by prose.
		{"bare-verb-no-pickup", "list_dir\nThen I will inspect the agent package.", "list_dir", ""},
	})
}
