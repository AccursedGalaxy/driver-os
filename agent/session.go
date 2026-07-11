package agent

import (
	"context"
	"sync"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// LoopFunc is the shared signature of the two agent loops, Run (text protocol)
// and RunNative (native tool-calling). A Session is parameterized by one so the
// same multi-turn machinery drives either — the caller picks the loop that fits
// its provider exactly as cmd/agent already does.
type LoopFunc func(context.Context, Config) (*RunResult, error)

// verifyBaselineCache retains the pre-flight result for a Session. Its tree key
// makes a cached result valid only while the complete working tree is unchanged.
type verifyBaselineCache struct {
	mu       sync.Mutex
	tree     string
	cmd      string
	measured bool
	red      bool
	out      string
	infra    string
}

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
	cfg            Config        // base config; Task and History are set per Send, the rest is fixed.
	loop           LoopFunc      // Run or RunNative.
	messages       []llm.Message // the full conversation carried across turns.
	verifyBaseline *verifyBaselineCache

	// autoVerifyProbe is separate from cfg: its goroutine only writes this
	// mutex-protected slot, never the session's base configuration.
	autoVerifyProbe struct {
		sync.Mutex
		started, done, applied, waitingNoted bool
		resolution                           autoVerifyResolution
	}
}

// NewSession returns a Session that runs each turn through loop with cfg as the
// fixed base (Model, Sandbox, Memory, Tools, the termination knobs …). The
// Task and History fields of cfg are ignored — Send sets them per turn. A nil
// loop defaults to Run (the text protocol); pass RunNative for native tool use.
func NewSession(cfg Config, loop LoopFunc) *Session {
	return NewSessionWith(cfg, loop, nil)
}

// NewSessionWith is NewSession plus an explicit conversation seed. The seed is
// used by resume paths that loaded a prior RunRecord.Messages transcript.
func NewSessionWith(cfg Config, loop LoopFunc, history []llm.Message) *Session {
	if loop == nil {
		loop = Run
	}
	return &Session{cfg: cfg, loop: loop, messages: append([]llm.Message(nil), history...), verifyBaseline: &verifyBaselineCache{}}
}

// PrewarmAutoVerify starts the automatic verify derivation and untouched-tree
// baseline in the background. It is intentionally optional: callers that do
// not prewarm retain the synchronous auto-verify behavior of Send.
func (s *Session) PrewarmAutoVerify(ctx context.Context) {
	if !s.cfg.AutoVerify || s.cfg.VerifyCmd != "" || s.cfg.Root == "" || s.cfg.MinIsolation != sandbox.IsolationNone {
		return
	}

	s.autoVerifyProbe.Lock()
	if s.autoVerifyProbe.started {
		s.autoVerifyProbe.Unlock()
		return
	}
	s.autoVerifyProbe.started = true
	snapshot := s.cfg
	s.autoVerifyProbe.Unlock()

	go func() {
		resolution := probeAutoVerify(context.WithoutCancel(ctx), snapshot)
		s.autoVerifyProbe.Lock()
		s.autoVerifyProbe.resolution = resolution
		s.autoVerifyProbe.done = true
		s.autoVerifyProbe.Unlock()
	}()
}

// Send runs one user turn to completion and returns its result. The conversation
// grows: this turn's input, every assistant turn, and every tool result are
// retained so the next Send continues from them. On a result that reached the
// loop (Answered, a cap, a spiral kill — anything with a populated transcript)
// the history advances; a pre-loop refusal or a result with no Messages leaves
// the prior history intact, so a transient failure doesn't truncate the chat.
func (s *Session) Send(ctx context.Context, input string) (*RunResult, error) {
	return s.SendParts(ctx, input, nil)
}

// SendParts runs one user turn with explicit content parts: text plus optional
// images. The text remains the task projection used by recall, memory, planning,
// and RunResult; images are attached only to this turn's user message.
func (s *Session) SendParts(ctx context.Context, text string, images []llm.ImagePart) (*RunResult, error) {
	cfg := s.cfg

	// A prewarm is deliberately non-blocking. While it is in flight, marking the
	// per-turn copy resolved prevents the loop from doing the old synchronous
	// preflight; the first turn after completion receives the saved resolution.
	var apply *autoVerifyResolution
	var waiting bool
	s.autoVerifyProbe.Lock()
	if s.autoVerifyProbe.started {
		if s.autoVerifyProbe.done {
			if !s.autoVerifyProbe.applied {
				resolution := s.autoVerifyProbe.resolution
				apply = &resolution
				s.autoVerifyProbe.applied = true
			}
		} else {
			cfg.autoVerifyResolved = true
			if !s.autoVerifyProbe.waitingNoted {
				s.autoVerifyProbe.waitingNoted = true
				waiting = true
			}
		}
	}
	s.autoVerifyProbe.Unlock()
	if apply != nil {
		applyAutoVerifyResolution(&s.cfg, *apply, false)
		applyAutoVerifyResolution(&cfg, *apply, true)
	}
	if waiting && cfg.Obs != nil {
		cfg.Obs.Note("auto-verify: baseline probe still running in the background — this turn runs without the soft gate")
	}

	cfg.Task = text
	cfg.TaskImages = images
	cfg.History = s.messages
	cfg.verifyBaselineCache = s.verifyBaseline
	res, err := s.loop(ctx, cfg)
	s.persistAutoVerify(res)
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

func (s *Session) persistAutoVerify(res *RunResult) {
	if res == nil || !res.autoVerifyResolved {
		return
	}
	// Approach B: keep auto-verify lazy in Run/RunNative, then persist the one
	// resolution back into the Session base config. Later SendParts copies a cfg
	// where either VerifyCmd is already armed (natural resolveAutoVerify no-op) or
	// autoVerifyResolved records that auto-verify was already decided off; both
	// avoid re-deriving and re-running the preflight on a WIP tree.
	s.cfg.autoVerifyResolved = true
	if res.autoVerifyCmd == "" {
		return
	}
	s.cfg.VerifyCmd = res.autoVerifyCmd
	s.cfg.AutoVerifySoft = res.autoVerifySoft
	s.cfg.autoVerifyProvenance = res.autoVerifyProvenance
	s.cfg.VerifyContinue = res.autoVerifyVerifyContinue
	s.cfg.SkipVerifyBaseline = res.autoVerifySkipVerifyBaseline
}

// Reset clears the conversation, starting fresh on the next Send (the /clear
// command). The warm Sandbox and Memory are untouched — only the dialogue resets.
func (s *Session) Reset() { s.messages = nil }
