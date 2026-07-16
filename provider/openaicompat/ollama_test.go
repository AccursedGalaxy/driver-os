package openaicompat

import "testing"

func TestOllamaBaseURL(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{
			name: "unset or empty",
			want: "http://localhost:11434/v1",
		},
		{
			name: "bare host",
			host: "ollama.internal",
			want: "http://ollama.internal:11434/v1",
		},
		{
			name: "host with port",
			host: "ollama.internal:8080",
			want: "http://ollama.internal:8080/v1",
		},
		{
			name: "http URL",
			host: "http://ollama.internal",
			want: "http://ollama.internal:11434/v1",
		},
		{
			name: "https URL without port",
			host: "https://ollama.internal",
			want: "https://ollama.internal:443/v1",
		},
		{
			name: "trailing slash",
			host: "http://ollama.internal:8080/",
			want: "http://ollama.internal:8080/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ollamaBaseURL(tt.host); got != tt.want {
				t.Errorf("ollamaBaseURL(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}
