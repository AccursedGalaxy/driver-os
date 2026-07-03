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

// ImagePart is image content for vision-capable models.
type ImagePart struct {
	// URL is a remote image reference. Mutually exclusive with Data.
	URL string
	// Data is raw image bytes. Mutually exclusive with URL.
	Data []byte
	// MIME is the media type for Data, e.g. "image/png".
	MIME string
}

func (ImagePart) isContentPart() {}

// ImageData is a convenience constructor for an ImagePart with raw bytes.
func ImageData(mime string, data []byte) ImagePart {
	return ImagePart{MIME: mime, Data: data}
}

// ImageURL is a convenience constructor for an ImagePart referencing a remote URL.
func ImageURL(url string) ImagePart {
	return ImagePart{URL: url}
}

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

// ReasoningPart carries a provider's OPAQUE reasoning trace for an assistant turn
// (OpenRouter's `reasoning_details`), so the loop can replay it verbatim when it
// re-sends that turn. It exists for ENCRYPTED reasoning that is STATEFUL: Gemini
// returns its chain of thought as an encrypted "thought signature"
// (reasoning.encrypted / google-gemini-v1) and its API requires that signature be
// sent back on the following turn — without it the model is amnesiac across tool
// calls and spirals (re-issuing the same action until a no-progress detector kills
// it). Plaintext reasoning (e.g. deepseek's reasoning.text) is carried too, but is
// re-derivable so dropping it is harmless; the encrypted kind is not. The harness
// never INTERPRETS Raw — it only round-trips it, so the field stays a provider
// black box.
type ReasoningPart struct{ Raw json.RawMessage }

func (ReasoningPart) isContentPart() {}

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
