package mockmodel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewReportsScriptErrors(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("New accepted a missing script")
	}
	path := writeScript(t, "not json")
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "parse mock script") {
		t.Fatalf("New invalid script error = %v", err)
	}
}

func TestStreamChunksInOrder(t *testing.T) {
	p, err := New(writeScript(t, `{"calls":[{"chunks":["Hello ","world."]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var chunks []string
	var done llm.Chunk
	for chunk, err := range p.Stream(context.Background(), llm.Request{}) {
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Done {
			done = chunk
		} else {
			chunks = append(chunks, chunk.Text)
		}
	}
	if got := strings.Join(chunks, ""); got != "Hello world." {
		t.Errorf("text = %q", got)
	}
	if done.Usage.TotalTokens == 0 {
		t.Error("terminal usage was empty")
	}
}

func TestStreamCancellationStopsPromptly(t *testing.T) {
	p, err := New(writeScript(t, `{"calls":[{"chunk_delay_ms":25,"chunks":["first","second"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	seenFirst := false
	for chunk, err := range p.Stream(ctx, llm.Request{}) {
		if err != nil {
			if err != context.Canceled {
				t.Fatalf("stream error = %v", err)
			}
			break
		}
		if chunk.Text == "first" {
			seenFirst = true
			cancel()
		}
	}
	if !seenFirst {
		t.Fatal("first chunk was not emitted")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled stream took %v", elapsed)
	}
}

func TestRunNativeExecutesScriptedToolCall(t *testing.T) {
	p, err := New(writeScript(t, `{"calls":[{"tool_calls":[{"name":"bash","args":{"command":"echo hi"}}]},{"chunks":["All done."]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	var command string
	res, err := agent.RunNative(context.Background(), agent.Config{
		Model: p, Sandbox: sb, Task: "test", Stream: true,
		Tools: map[string]agent.Tool{
			"bash": {Name: "bash", RunJSON: func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				command = in.Command
				return "hi", nil
			}},
		},
	})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if command != "echo hi" {
		t.Errorf("tool command = %q, want echo hi", command)
	}
	if res.Answer != "All done." {
		t.Errorf("answer = %q, want All done.", res.Answer)
	}
}

func TestCallIndexClampsToLastEntry(t *testing.T) {
	p, err := New(writeScript(t, `{"calls":[{"chunks":["one"]},{"chunks":["two"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"one", "two", "two"} {
		resp, err := p.Generate(context.Background(), llm.Request{})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Text(); got != want {
			t.Errorf("response = %q, want %q", got, want)
		}
	}
}
