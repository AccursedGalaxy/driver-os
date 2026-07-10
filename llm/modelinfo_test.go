package llm

import "testing"

func TestLookupModelInfo(t *testing.T) {
	tests := []struct {
		model  string
		window int
	}{
		{"gpt-5.6-luna", 1_050_000},
		{"openai/gpt-5.6-terra", 1_050_000},
		{"claude-fable-5", 200_000},
		{"anthropic/claude-opus-4.8", 200_000},
		{"openai/gpt-5.5", 0},
		{"unknown/model", 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := Lookup(tt.model)
			if got.ContextWindow != tt.window {
				t.Fatalf("Lookup(%q).ContextWindow = %d, want %d", tt.model, got.ContextWindow, tt.window)
			}
			if got.MaxOutputTokens != 0 {
				t.Fatalf("Lookup(%q).MaxOutputTokens = %d, want unknown (zero)", tt.model, got.MaxOutputTokens)
			}
		})
	}
}
