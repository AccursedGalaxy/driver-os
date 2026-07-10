// Package mockmodel provides a deterministic, script-driven LLM provider for
// exercising the driver UI without credentials or network access.
//
// A script is JSON in the form:
//
//	{"calls":[{"pre_delay_ms":300,"chunk_delay_ms":20,"chunks":["Hello ","world."],"tool_calls":[{"name":"bash","args":{"command":"echo hi"}}]},{"chunks":["All done."]}]}
//
// Each request consumes the next calls entry; requests after the final entry
// repeat that entry. Stream emits the configured text chunks, then complete
// native tool calls, and finally a terminal chunk with usage.
package mockmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AccursedGalaxy/driver-os/llm"
)

type script struct {
	Calls []call `json:"calls"`
}

type call struct {
	PreDelayMS   int        `json:"pre_delay_ms"`
	ChunkDelayMS int        `json:"chunk_delay_ms"`
	Chunks       []string   `json:"chunks"`
	ToolCalls    []toolCall `json:"tool_calls"`
}

type toolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Provider replays the calls loaded from a mock script.
type Provider struct {
	path  string
	calls []call

	mu   sync.Mutex
	next int
}

var _ llm.Provider = (*Provider)(nil)

// New reads and validates scriptPath before constructing a deterministic provider.
func New(scriptPath string) (llm.Provider, error) {
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("read mock script %q: %w", scriptPath, err)
	}
	var s script
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse mock script %q: %w", scriptPath, err)
	}
	if len(s.Calls) == 0 {
		return nil, fmt.Errorf("parse mock script %q: calls must not be empty", scriptPath)
	}
	for i, c := range s.Calls {
		if c.PreDelayMS < 0 || c.ChunkDelayMS < 0 {
			return nil, fmt.Errorf("parse mock script %q: calls[%d] has a negative delay", scriptPath, i)
		}
		for j, tc := range c.ToolCalls {
			if tc.Name == "" {
				return nil, fmt.Errorf("parse mock script %q: calls[%d].tool_calls[%d] has no name", scriptPath, i, j)
			}
			if len(tc.Args) == 0 || !json.Valid(tc.Args) {
				return nil, fmt.Errorf("parse mock script %q: calls[%d].tool_calls[%d] has invalid args", scriptPath, i, j)
			}
		}
	}
	return &Provider{path: scriptPath, calls: s.Calls}, nil
}

func (p *Provider) Name() string { return "mock" }

// Model supplies the banner-friendly model label expected by cmd/driver.
func (p *Provider) Model() string { return "mock:" + filepath.Base(p.path) }

func (p *Provider) Capabilities() llm.Capabilities {
	return llm.Capabilities{Streaming: true, Tools: true}
}

func (p *Provider) take() (call, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.next
	p.next++
	if i >= len(p.calls) {
		i = len(p.calls) - 1
	}
	return p.calls[i], i
}

func responseFor(c call, callIndex int) *llm.Response {
	parts := make([]llm.ContentPart, 0, len(c.Chunks)+len(c.ToolCalls))
	text := ""
	for _, chunk := range c.Chunks {
		text += chunk
	}
	if text != "" {
		parts = append(parts, llm.Text(text))
	}
	for i, tc := range c.ToolCalls {
		parts = append(parts, llm.ToolCallPart{
			ID: fmt.Sprintf("mock-%d-%d", callIndex+1, i+1), Name: tc.Name, Args: tc.Args,
		})
	}
	finish := llm.FinishStop
	if len(c.ToolCalls) > 0 {
		finish = llm.FinishToolUse
	}
	return &llm.Response{Content: parts, FinishReason: finish, Usage: usage()}
}

func usage() llm.Usage { return llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15} }

// Generate returns the next scripted response without applying timing delays.
func (p *Provider) Generate(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, i := p.take()
	return responseFor(c, i), nil
}

// Stream emits the next scripted response incrementally, observing cancellation
// while waiting for configured delays and before each emitted item.
func (p *Provider) Stream(ctx context.Context, _ llm.Request) iter.Seq2[llm.Chunk, error] {
	c, callIndex := p.take()
	return func(yield func(llm.Chunk, error) bool) {
		if !wait(ctx, time.Duration(c.PreDelayMS)*time.Millisecond) {
			yield(llm.Chunk{}, ctx.Err())
			return
		}
		for i, text := range c.Chunks {
			if err := ctx.Err(); err != nil {
				yield(llm.Chunk{}, err)
				return
			}
			if !yield(llm.Chunk{Text: text}, nil) {
				return
			}
			if i+1 < len(c.Chunks) && !wait(ctx, time.Duration(c.ChunkDelayMS)*time.Millisecond) {
				yield(llm.Chunk{}, ctx.Err())
				return
			}
		}
		for i, tc := range c.ToolCalls {
			if err := ctx.Err(); err != nil {
				yield(llm.Chunk{}, err)
				return
			}
			part := &llm.ToolCallPart{ID: fmt.Sprintf("mock-%d-%d", callIndex+1, i+1), Name: tc.Name, Args: tc.Args}
			if !yield(llm.Chunk{ToolCall: part}, nil) {
				return
			}
		}
		finish := llm.FinishStop
		if len(c.ToolCalls) > 0 {
			finish = llm.FinishToolUse
		}
		if ctx.Err() != nil {
			yield(llm.Chunk{}, ctx.Err())
			return
		}
		yield(llm.Chunk{Done: true, FinishReason: finish, Usage: usage()}, nil)
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	if d == 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
