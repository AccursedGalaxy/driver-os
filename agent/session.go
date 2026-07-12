package agent

import (
	"context"
	"sync"

	"github.com/AccursedGalaxy/driver-os/runspec"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// LoopFunc is the shared signature of the two agent loops, Run (text protocol)
// and RunNative (native tool-calling). A Session is parameterized by one so the
// same multi-turn machinery drives either — the caller picks the loop that fits
// its provider.
type LoopFunc func(context.Context, runspec.ResolvedSpec, Runtime, Content) (*RunResult, error)

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
// warm Sandbox and one open Memory across the conversation (they ride in the
// Runtime and are passed unchanged to every loop call). The Session also owns the
// REQUESTED side of policy (runspec.RequestedConfig): live reconfiguration
// (SetMaxIterations, /model-adjacent commands) edits the request and re-resolves,
// so there is still exactly one place a policy number can come from — Resolve.
//
// Session is NOT safe for concurrent Sends; a conversation is inherently serial
// (each turn depends on the last). Drive it from one goroutine.
type Session struct {
	req      runspec.RequestedConfig // requested-side policy; setters edit + re-Resolve.
	spec     runspec.ResolvedSpec    // the resolved spec every Send runs under.
	rt       Runtime                 // fixed bindings (Model, Sandbox, Memory, Tools, …); setters may swap members.
	root     string                  // workspace root (Content.Root for every turn).
	loop     LoopFunc                // Run or RunNative.
	messages []llm.Message           // the full conversation carried across turns.

	// vs is the session's persisted verification state: the spec's verification
	// domain plus the session's one-time auto-verify decision and any /verify
	// override. Each Send hands the loop a COPY; the loop's own resolution flows
	// back only through the explicit RunResult hand-off (persistAutoVerify).
	vs             verifyState
	verifyBaseline *verifyBaselineCache

	// autoVerifyProbe is separate from vs: its goroutine only writes this
	// mutex-protected slot, never the session's persisted state.
	autoVerifyProbe struct {
		sync.Mutex
		started, done, applied, waitingNoted bool
		resolution                           autoVerifyResolution
	}
}

// NewSession resolves req once and returns a Session that runs each turn
// through loop with the resolved spec and rt as the fixed base. root is the
// workspace root every turn's Content carries. A nil loop defaults to Run (the
// text protocol); pass RunNative for native tool use.
func NewSession(req runspec.RequestedConfig, rt Runtime, root string, loop LoopFunc) (*Session, error) {
	return NewSessionWith(req, rt, root, loop, nil)
}

// NewSessionWith is NewSession plus an explicit conversation seed. The seed is
// used by resume paths that loaded a prior RunRecord.Messages transcript.
func NewSessionWith(req runspec.RequestedConfig, rt Runtime, root string, loop LoopFunc, history []llm.Message) (*Session, error) {
	if loop == nil {
		loop = Run
	}
	spec, _, err := runspec.Resolve(req)
	if err != nil {
		return nil, err
	}
	return &Session{
		req:            req,
		spec:           spec,
		rt:             rt,
		root:           root,
		loop:           loop,
		messages:       append([]llm.Message(nil), history...),
		vs:             *newVerifyState(spec.Policy()),
		verifyBaseline: &verifyBaselineCache{},
	}, nil
}

// Spec returns the resolved spec the next Send will run under.
func (s *Session) Spec() runspec.ResolvedSpec { return s.spec }

// NewSessionFromConfig builds a Session from a Config template — the
// requested-side compatibility constructor for callers whose builders still
// assemble Config (deleted with Config in S6b). history may be nil.
func NewSessionFromConfig(cfg Config, loop LoopFunc, history []llm.Message) (*Session, error) {
	rt, content := cfg.Bindings()
	return NewSessionWith(cfg.Requested(), rt, content.Root, loop, history)
}

// reresolve applies a mutation to the requested config and re-resolves. The
// setters this backs are documented caller-validates (they always were); a
// resolution failure keeps the previous spec so the session stays usable.
func (s *Session) reresolve(mutate func(*runspec.RequestedConfig)) {
	req := s.req
	mutate(&req)
	spec, _, err := runspec.Resolve(req)
	if err != nil {
		if s.rt.Obs != nil {
			s.rt.Obs.Note("session: rejected live reconfiguration (" + err.Error() + ") — keeping the previous spec")
		}
		return
	}
	s.req, s.spec = req, spec
}

// PrewarmAutoVerify starts the automatic verify derivation and untouched-tree
// baseline in the background. It is intentionally optional: callers that do
// not prewarm retain the synchronous auto-verify behavior of Send.
func (s *Session) PrewarmAutoVerify(ctx context.Context) {
	pol := s.spec.Policy()
	if !s.vs.AutoVerify || s.vs.Cmd != "" || s.root == "" || pol.MinIsolation != sandbox.IsolationNone {
		return
	}

	s.autoVerifyProbe.Lock()
	if s.autoVerifyProbe.started {
		s.autoVerifyProbe.Unlock()
		return
	}
	s.autoVerifyProbe.started = true
	verifySB, root := s.rt.verifySandbox(), s.root
	s.autoVerifyProbe.Unlock()

	go func() {
		resolution := probeAutoVerify(context.WithoutCancel(ctx), verifySB, root)
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
	turnVS := s.vs

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
			turnVS.resolved = true
			if !s.autoVerifyProbe.waitingNoted {
				s.autoVerifyProbe.waitingNoted = true
				waiting = true
			}
		}
	}
	s.autoVerifyProbe.Unlock()
	if apply != nil {
		applyAutoVerifyResolution(&s.vs, *apply, s.rt.Obs, false)
		applyAutoVerifyResolution(&turnVS, *apply, s.rt.Obs, true)
	}
	if waiting && s.rt.Obs != nil {
		s.rt.Obs.Note("auto-verify: baseline probe still running in the background — this turn runs without the soft gate")
	}

	rt := s.rt
	rt.verifyBaselineCache = s.verifyBaseline
	rt.sessionVerify = &turnVS
	content := Content{Task: text, TaskImages: images, History: s.messages, Root: s.root}
	res, err := s.loop(ctx, s.spec, rt, content)
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
	// resolution back into the Session verification state. Later SendParts hand
	// the loop a state where either Cmd is already armed (natural
	// resolveAutoVerify no-op) or resolved records that auto-verify was already
	// decided off; both avoid re-deriving and re-running the preflight on a WIP
	// tree.
	s.vs.resolved = true
	if res.autoVerifyCmd == "" {
		return
	}
	s.vs.Cmd = res.autoVerifyCmd
	s.vs.AutoVerifySoft = res.autoVerifySoft
	s.vs.provenance = res.autoVerifyProvenance
	s.vs.VerifyContinue = res.autoVerifyVerifyContinue
	s.vs.SkipVerifyBaseline = res.autoVerifySkipVerifyBaseline
}

// Reset clears the conversation, starting fresh on the next Send (the /clear
// command). The warm Sandbox and Memory are untouched — only the dialogue resets.
func (s *Session) Reset() { s.messages = nil }
