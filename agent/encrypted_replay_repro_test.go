package agent

// Repro for the gpt-5.6 `invalid_encrypted_content` replay kill (run
// 20260709-214235, backlog 2026-07-09): OpenAI 400s while re-verifying its
// OWN encrypted reasoning item on replay ("Encrypted content could not be
// decrypted or parsed", rs_...). The error is deterministic for the
// conversation — a verbatim retry can never succeed — so today the run dies
// ProviderErr/exit 5. Reasoning replay is an optimization, not a correctness
// requirement: the loop should drop the poisoned reasoning item(s) from the
// replayed history and retry the turn.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// poisonedReplayModel emulates the production failure: the scripted turns can
// emit encrypted reasoning items, and every Generate whose request REPLAYS a
// ReasoningPart is rejected with the exact error shape openaicompat.classify
// produces for this 400 today (KindUnknown, Retryable:false). A request with
// the reasoning stripped succeeds.
type poisonedReplayModel struct {
	nativeScript
	rejections int
}

func (m *poisonedReplayModel) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	for _, msg := range req.Messages {
		if msg.Role != llm.RoleAssistant {
			continue
		}
		for _, p := range msg.Parts {
			if _, ok := p.(llm.ReasoningPart); ok {
				m.rejections++
				return nil, &llm.ProviderError{
					Provider:   "openaicompat",
					StatusCode: 400,
					Retryable:  false,
					Err:        errors.New("Encrypted content could not be decrypted or parsed: rs_0115882deadbeef"),
				}
			}
		}
	}
	return m.nativeScript.Generate(ctx, req)
}

func TestEncryptedReplayRejectionRecovers(t *testing.T) {
	turns := [][]llm.ContentPart{
		{
			llm.ReasoningPart{Raw: json.RawMessage(`{"type":"reasoning.encrypted","data":"poisoned"}`)},
			structuredCall("c1", "read_file", map[string]any{"path": "a"}),
		},
		{structuredCall("c2", "read_file", map[string]any{"path": "b"})},
		{llm.Text("done")},
	}
	m := &poisonedReplayModel{nativeScript: nativeScript{turns: turns}}
	sb := sbWith(t, map[string]string{"a": "1\n", "b": "2\n"})
	res, err := RunNative(context.Background(), Config{
		Model:         m,
		Sandbox:       sb,
		Task:          "test task",
		MaxIterations: 10,
	})
	if err != nil {
		t.Fatalf("RunNative error — the run died on the poisoned replay instead of recovering: %v", err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s), want Answered — the loop must strip the poisoned reasoning item from replay and retry the turn", res.Outcome, res.Reason)
	}
	// Exactly one rejection: the first replay attempt. After recovery the
	// scrub must persist in the loop's history — if only the single request
	// were stripped, the NEXT turn would replay the poisoned item and be
	// rejected again (the production error is deterministic per conversation).
	if m.rejections != 1 {
		t.Fatalf("provider rejected %d replay(s), want exactly 1 — poisoned reasoning items must stay out of the history after recovery", m.rejections)
	}
}
