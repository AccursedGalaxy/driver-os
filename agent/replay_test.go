package agent

import (
	"reflect"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

func TestScrubReplayReasoningStripsOnlyAssistantReasoning(t *testing.T) {
	msgs := []llm.Message{
		llm.User("question"),
		{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.TextPart{Text: "answer"}, llm.ReasoningPart{Raw: []byte("sealed")}, llm.ToolCallPart{ID: "call", Name: "read"}}},
		llm.ToolResultMsg("call", "result", false),
		{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.ReasoningPart{Raw: []byte("untouched")}}},
	}
	got := ScrubReplayReasoning(msgs)
	if len(got[1].Parts) != 2 {
		t.Fatalf("assistant parts = %#v, want reasoning removed", got[1].Parts)
	}
	if _, ok := got[1].Parts[0].(llm.TextPart); !ok {
		t.Fatalf("assistant text changed: %#v", got[1].Parts[0])
	}
	if _, ok := got[1].Parts[1].(llm.ToolCallPart); !ok {
		t.Fatalf("assistant tool call changed: %#v", got[1].Parts[1])
	}
	if !reflect.DeepEqual(got[0], msgs[0]) || !reflect.DeepEqual(got[2], msgs[2]) || !reflect.DeepEqual(got[3], msgs[3]) {
		t.Fatalf("non-assistant content changed: %#v", got)
	}
}
