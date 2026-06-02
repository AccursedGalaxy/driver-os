package llm

import (
	"encoding/json"
	"strings"
)

// ContentPart is a single piece of a message's content. It is a closed sum
// type: the only implementations are those defined in this package (TextPart,
// ImagePart, ToolCallPart, ToolResultPart).
//
// The content model is part-based from day one (decision 3) because both
// underlying wire formats are natively part-based and because vision and tool
// use require it. Text-first helpers keep the common case a one-liner.
type ContentPart interface {
	isContentPart()
}

// TextPart is plain text content.
type TextPart struct{ Text string }

func (TextPart) isContentPart() {}

// ImagePart is image content for vision-capable models. Reserved now so the
// content model is vision-ready; not yet wired through the adapters.
type ImagePart struct {
	// URL is a remote image reference. Mutually exclusive with Data.
	URL string
	// Data is raw image bytes. Mutually exclusive with URL.
	Data []byte
	// MIME is the media type for Data, e.g. "image/png".
	MIME string
}

func (ImagePart) isContentPart() {}

// ToolCallPart is the model asking to invoke a tool: a provider-assigned call ID,
// the tool Name, and the raw JSON Args object. It appears in an assistant message
// when FinishReason is FinishToolUse. The caller runs the named tool and answers
// with a ToolResultPart carrying the same ID.
type ToolCallPart struct {
	ID   string
	Name string
	Args json.RawMessage
}

func (ToolCallPart) isContentPart() {}

// ToolResultPart is the caller's answer to a ToolCallPart: the observation text,
// tagged with the ToolCallID it answers and whether the tool failed. IsError is a
// real outcome the model should see and react to (a failed tool is information,
// not a transport error) — not a Go error. Carried in a RoleTool message.
type ToolResultPart struct {
	ToolCallID string
	Content    string
	IsError    bool
}

func (ToolResultPart) isContentPart() {}

// Text is a convenience constructor for a TextPart.
func Text(s string) TextPart { return TextPart{Text: s} }

// partsText concatenates the text of all TextParts in order, ignoring
// non-text parts. Used by Message.Text and Response.Text.
func partsText(parts []ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p.(TextPart); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
