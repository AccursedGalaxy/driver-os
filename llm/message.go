package llm

import (
	"encoding/json"
	"fmt"
)

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation: a role plus structured content parts.
type Message struct {
	Role  Role
	Parts []ContentPart
}

type messageWire struct {
	Role  Role              `json:"role"`
	Parts []contentPartWire `json:"parts,omitempty"`
}

type contentPartWire struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	URL        string          `json:"url,omitempty"`
	Data       []byte          `json:"data,omitempty"`
	MIME       string          `json:"mime,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Content    string          `json:"content,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// MarshalJSON gives Message a durable JSON shape despite Parts being an
// interface slice. Transcripts depend on being able to read conversations back
// into Config.History for session resume.
func (m Message) MarshalJSON() ([]byte, error) {
	parts := make([]contentPartWire, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch v := p.(type) {
		case TextPart:
			parts = append(parts, contentPartWire{Type: "text", Text: v.Text})
		case ImagePart:
			parts = append(parts, contentPartWire{Type: "image", URL: v.URL, Data: v.Data, MIME: v.MIME})
		case ToolCallPart:
			parts = append(parts, contentPartWire{Type: "tool_call", ID: v.ID, Name: v.Name, Args: v.Args})
		case ToolResultPart:
			parts = append(parts, contentPartWire{Type: "tool_result", ToolCallID: v.ToolCallID, Content: v.Content, IsError: v.IsError})
		case ReasoningPart:
			parts = append(parts, contentPartWire{Type: "reasoning", Raw: v.Raw})
		default:
			return nil, fmt.Errorf("llm: unsupported content part %T", p)
		}
	}
	return json.Marshal(messageWire{Role: m.Role, Parts: parts})
}

// UnmarshalJSON restores the concrete ContentPart implementations emitted by
// MarshalJSON.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w messageWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	parts := make([]ContentPart, 0, len(w.Parts))
	for _, p := range w.Parts {
		switch p.Type {
		case "text":
			parts = append(parts, TextPart{Text: p.Text})
		case "image":
			parts = append(parts, ImagePart{URL: p.URL, Data: p.Data, MIME: p.MIME})
		case "tool_call":
			parts = append(parts, ToolCallPart{ID: p.ID, Name: p.Name, Args: p.Args})
		case "tool_result":
			parts = append(parts, ToolResultPart{ToolCallID: p.ToolCallID, Content: p.Content, IsError: p.IsError})
		case "reasoning":
			parts = append(parts, ReasoningPart{Raw: p.Raw})
		default:
			return fmt.Errorf("llm: unknown content part type %q", p.Type)
		}
	}
	m.Role = w.Role
	m.Parts = parts
	return nil
}

// Text returns the concatenated text of the message's text parts.
func (m Message) Text() string { return partsText(m.Parts) }

// User builds a user message from a plain string (the 90% case).
func User(s string) Message { return Message{Role: RoleUser, Parts: []ContentPart{Text(s)}} }

// System builds a system message from a plain string.
func System(s string) Message { return Message{Role: RoleSystem, Parts: []ContentPart{Text(s)}} }

// Assistant builds an assistant message from a plain string.
func Assistant(s string) Message {
	return Message{Role: RoleAssistant, Parts: []ContentPart{Text(s)}}
}

// UserParts builds a user message from explicit content parts (e.g. text +
// image), for when the string helper is not enough.
func UserParts(parts ...ContentPart) Message {
	return Message{Role: RoleUser, Parts: parts}
}

// ToolResultMsg builds a tool-result message answering the tool call with id.
// isErr marks a tool failure — still an observation the model acts on, not a
// transport error.
func ToolResultMsg(id, content string, isErr bool) Message {
	return Message{Role: RoleTool, Parts: []ContentPart{ToolResultPart{ToolCallID: id, Content: content, IsError: isErr}}}
}
