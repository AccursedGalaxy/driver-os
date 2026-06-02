package llm

import "encoding/json"

// Tool is a provider-agnostic description of a function the model may call. It
// carries NO handler — execution lives in the caller's loop (decision 4), which
// matches a returned ToolCallPart to its Tool by Name and runs it. Adapters
// translate Schema into their provider's tool format (OpenAI's
// `function.parameters`, Claude's `input_schema`), so the same Tool works across
// providers.
type Tool struct {
	// Name is the function name the model calls and the loop dispatches on.
	Name string
	// Description tells the model when and how to use the tool.
	Description string
	// Schema is a JSON Schema object describing the arguments. An empty schema
	// declares a no-argument tool.
	Schema json.RawMessage
}
