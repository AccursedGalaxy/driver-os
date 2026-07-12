// Package memory defines the agent core's long-term memory contract.
package memory

import (
	"context"
	"time"
)

// Scope is the namespace a memory belongs to.
type Scope struct {
	UserID  string `json:"user_id,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	RunID   string `json:"run_id,omitempty"`
}

// Message is a conversational turn supplied to Store.Add.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// Fact is a durable statement returned by a Store.
type Fact struct {
	ID        string
	Text      string
	Hash      string
	Score     float32
	CreatedAt time.Time
	Scope     Scope
}

// Store is the long-term memory functionality used by the agent and its lifecycle.
type Store interface {
	Add(ctx context.Context, msgs []Message, scope Scope) ([]Fact, error)
	Search(ctx context.Context, query string, scope Scope, k int) ([]Fact, error)
	Close() error
}
