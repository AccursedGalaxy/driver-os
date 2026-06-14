package agent

import (
	"context"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// LoopFunc is the shared signature of the two agent loops, Run (text protocol)
// and RunNative (native tool-calling). A Session is parameterized by one so the
// same multi-turn machinery drives either — the caller picks the loop that fits
// its provider exactly as cmd/agent already does.
type LoopFunc func(context.Context, Config) (*RunResult, error)

// Session is a CONTINUING conversation over the agent loop — the statefulness a
// chat front-end needs that single-shot Run/RunNative lack. Each Send runs one
// user turn to completion (the loop iterates think->act->observe internally until
// it answers or hits a cap) and folds the resulting transcript back in, so the
// next Send sees the whole prior exchange.
//
// The expensive context is held HERE, not rebuilt per turn: the Session keeps one
// warm Sandbox and one open Memory across the conversation (they ride in the base
// Config and are passed unchanged to every loop call). That is what makes a chat
// over -session/docker viable — the container stays up for the whole conversation
// instead of being torn down and re-created between user messages.
//
// Session is NOT safe for concurrent Sends; a conversation is inherently serial
// (each turn depends on the last). Drive it from one goroutine.
type Session struct {
	cfg      Config        // base config; Task and History are set per Send, the rest is fixed.
	loop     LoopFunc      // Run or RunNative.
	messages []llm.Message // the full conversation carried across turns.
}

// NewSession returns a Session that runs each turn through loop with cfg as the
// fixed base (Model, Sandbox, Memory, Tools, the termination knobs …). The
// Task and History fields of cfg are ignored — Send sets them per turn. A nil
// loop defaults to Run (the text protocol); pass RunNative for native tool use.
func NewSession(cfg Config, loop LoopFunc) *Session {
	if loop == nil {
		loop = Run
	}
	return &Session{cfg: cfg, loop: loop}
}

// Send runs one user turn to completion and returns its result. The conversation
// grows: this turn's input, every assistant turn, and every tool result are
// retained so the next Send continues from them. On a result that reached the
// loop (Answered, a cap, a spiral kill — anything with a populated transcript)
// the history advances; a pre-loop refusal or a result with no Messages leaves
// the prior history intact, so a transient failure doesn't truncate the chat.
func (s *Session) Send(ctx context.Context, input string) (*RunResult, error) {
	cfg := s.cfg
	cfg.Task = input
	cfg.History = s.messages
	res, err := s.loop(ctx, cfg)
	// On cancellation (Ctrl-C interrupting a turn, or a deadline) the transcript may
	// end mid-turn — an assistant tool-call whose results never came back — and
	// committing that would malform the next request's tool-call/result pairing. Leave
	// the history at the prior clean state so the next Send continues cleanly.
	if ctx.Err() != nil {
		return res, err
	}
	if res != nil && len(res.Messages) > 0 {
		s.messages = res.Messages
	}
	return res, err
}

// Messages returns the conversation accumulated so far. The returned slice is the
// Session's own backing store — treat it as read-only; the next Send replaces it.
func (s *Session) Messages() []llm.Message { return s.messages }

// Reset clears the conversation, starting fresh on the next Send (the /clear
// command). The warm Sandbox and Memory are untouched — only the dialogue resets.
func (s *Session) Reset() { s.messages = nil }
