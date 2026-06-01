package llm

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
