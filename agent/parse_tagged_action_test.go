package agent

import "testing"

// Tag-wrapped action recognition (rung-0 smoke 2026-07-17): at sampling
// temperatures above 0 the local Qwen model slides into its native chat
// template and emits text-loop actions wrapped in XML-ish tags:
//
//	<tool_call>
//	<read_file> provider/openaicompat/openaicompat.go
//	</read_file>
//	</tool_call>
//
// One smoke attempt burned 30/30 iterations on "no action recognized".
// parseAction must accept a KNOWN verb in `<verb> arg` form (optional
// trailing `</verb>` on the same line), while unknown tags (<tool_call>,
// <think>, html) stay unrecognized — fail-closed.

func parseTools() map[string]Tool {
	return map[string]Tool{
		"read_file": {Name: "read_file"},
		"edit_file": {Name: "edit_file"},
		"list_dir":  {Name: "list_dir"},
	}
}

func TestParseActionTagWrappedVerb(t *testing.T) {
	reply := "<tool_call>\n<read_file> provider/openaicompat/openaicompat.go\n</read_file>\n</tool_call>"
	v, arg := parseAction(reply, parseTools())
	if v != "read_file" || arg != "provider/openaicompat/openaicompat.go" {
		t.Fatalf("got (%q, %q), want (read_file, path)", v, arg)
	}
}

func TestParseActionTagWrappedSameLineClose(t *testing.T) {
	v, arg := parseAction("<list_dir> provider </list_dir>", parseTools())
	if v != "list_dir" || arg != "provider" {
		t.Fatalf("got (%q, %q), want (list_dir, provider)", v, arg)
	}
}

func TestParseActionPlainFormStillWins(t *testing.T) {
	// The documented grammar must keep priority: a plain verb-initial line
	// earlier in the reply beats a tag-wrapped one later.
	v, arg := parseAction("read_file a.go\n<read_file> b.go\n</read_file>", parseTools())
	if v != "read_file" || arg != "a.go" {
		t.Fatalf("got (%q, %q), want (read_file, a.go)", v, arg)
	}
}

func TestParseActionUnknownTagsStayUnrecognized(t *testing.T) {
	for _, reply := range []string{
		"<tool_call>\n</tool_call>",       // wrapper with no verb inside
		"<think> I should read the file ", // non-verb tag
		"<div> read_file a.go </div>",     // html-ish, verb not tag name
		"<read_file>",                     // verb tag, no argument on the line
	} {
		if v, _ := parseAction(reply, parseTools()); v != "" {
			t.Fatalf("reply %q parsed as %q, want unrecognized", reply, v)
		}
	}
}
