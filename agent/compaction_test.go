package agent

import (
	"context"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// roleSeq renders a message slice as its role sequence (e.g. "u a t t a t") so a
// test can assert eviction kept pairing and order without comparing every part.
func roleSeq(msgs []llm.Message) []llm.Role {
	out := make([]llm.Role, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

func TestEvictOldestTurn(t *testing.T) {
	A := func() llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.Text("x")}}
	}
	tool := func(id string) llm.Message { return llm.ToolResultMsg(id, "obs", false) }

	cases := []struct {
		name     string
		in       []llm.Message
		wantOK   bool
		wantRole []llm.Role
	}{
		{
			// Text loop: TASK + two turns of Assistant+OBSERVATION. Drop the oldest
			// turn (A1+O1), keep TASK and the recent turn.
			name:     "text loop drops oldest turn",
			in:       []llm.Message{llm.User("TASK"), A(), llm.User("O1"), A(), llm.User("O2")},
			wantOK:   true,
			wantRole: []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser},
		},
		{
			// Native loop: an assistant turn can fan out to SEVERAL tool results. The
			// whole [assistant, tool, tool] span is the eviction unit — never a lone
			// RoleTool, which would be malformed without its preceding tool_calls.
			name:     "native loop drops assistant + all its tool results",
			in:       []llm.Message{llm.User("TASK"), A(), tool("a"), tool("b"), A(), tool("c")},
			wantOK:   true,
			wantRole: []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool},
		},
		{
			// Only TASK + one turn: nothing safe to drop (would lose the most recent
			// grounding), so report false and leave the slice untouched.
			name:   "single turn is not evictable",
			in:     []llm.Message{llm.User("TASK"), A(), llm.User("O1")},
			wantOK: false,
		},
		{
			// No assistant turn yet (only the TASK): nothing to evict.
			name:   "no assistant turn",
			in:     []llm.Message{llm.User("TASK")},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := evictOldestTurn(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				// On false the input must be returned unchanged (same length).
				if len(got) != len(tc.in) {
					t.Fatalf("not-ok path mutated the slice: len %d, want %d", len(got), len(tc.in))
				}
				return
			}
			gotRoles := roleSeq(got)
			if len(gotRoles) != len(tc.wantRole) {
				t.Fatalf("roles = %v, want %v", gotRoles, tc.wantRole)
			}
			for i := range gotRoles {
				if gotRoles[i] != tc.wantRole[i] {
					t.Fatalf("roles = %v, want %v", gotRoles, tc.wantRole)
				}
			}
			// The first message (TASK) must always survive.
			if got[0].Role != llm.RoleUser {
				t.Errorf("TASK message was dropped: first role = %q", got[0].Role)
			}
		})
	}
}

// ctxLimitModel simulates a finite context window: Generate returns
// llm.ErrContextLength whenever a request carries more than windowMsgs messages,
// otherwise it serves the next scripted reply. It lets a test drive HP-1's
// reactive eviction deterministically, with no real provider.
type ctxLimitModel struct {
	windowMsgs int      // overflow above this many messages.
	replies    []string // served on each NON-overflow call, in order (clamped to last).
	served     int      // count of non-overflow Generates.
	overflows  int      // count of overflow Generates.
	lastCall   llm.Request
}

func (m *ctxLimitModel) Name() string                   { return "ctxlimit" }
func (m *ctxLimitModel) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (m *ctxLimitModel) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	m.lastCall = req
	if len(req.Messages) > m.windowMsgs {
		m.overflows++
		return nil, llm.ErrContextLength
	}
	i := m.served
	if i >= len(m.replies) {
		i = len(m.replies) - 1
	}
	m.served++
	return &llm.Response{
		Content: []llm.ContentPart{llm.Text(m.replies[i])},
		Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

// On overflow with history present, the loop must evict the oldest turn and
// RETRY rather than crash — and carry the shrunk transcript forward.
func TestRunEvictsAndRetriesOnOverflow(t *testing.T) {
	// window=4 admits TASK and two turns; the third turn (5 msgs) overflows, gets
	// the oldest turn evicted back to 3 msgs, and then succeeds.
	m := &ctxLimitModel{
		windowMsgs: 4,
		replies:    []string{"list_dir .", "list_dir .", "answer done"},
	}
	res, err := Run(context.Background(), Config{
		Model:   m,
		Sandbox: sbWith(t, map[string]string{"go.mod": "module x\n"}),
		Task:    "test task",
	})
	if err != nil {
		t.Fatalf("Run returned an unexpected error: %v", err)
	}
	if m.overflows == 0 {
		t.Fatalf("expected at least one overflow to exercise eviction, got none")
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q, want %q (Reason: %s)", res.Outcome, Answered, res.Reason)
	}
	// The eviction must have kept the transcript within the window on the final call.
	if n := len(m.lastCall.Messages); n > m.windowMsgs {
		t.Errorf("final request carried %d messages, over the %d window — eviction did not stick", n, m.windowMsgs)
	}
}

// When the window is so small that even TASK + one turn overflows, eviction has
// nothing left to drop: the run must DEGRADE to HitContextLimit, not crash.
func TestRunDegradesWhenUncompactable(t *testing.T) {
	m := &ctxLimitModel{
		windowMsgs: -1, // len(req.Messages) > -1 is always true -> every call overflows.
		replies:    []string{"list_dir ."},
	}
	res, err := Run(context.Background(), Config{
		Model:   m,
		Sandbox: sbWith(t, nil),
		Task:    "test task",
	})
	if err != nil {
		t.Fatalf("Run should degrade gracefully, not return an error: %v", err)
	}
	if res.Outcome != HitContextLimit {
		t.Fatalf("Outcome = %q, want %q", res.Outcome, HitContextLimit)
	}
}
