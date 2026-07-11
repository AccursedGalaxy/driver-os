package agent

import (
	"github.com/AccursedGalaxy/driver-os/internal/runspec"
	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// This file extends Session with live reconfiguration for interactive
// front-ends (cmd/cc's /model command). It lives in its own file so the core
// Session (session.go) stays exactly the multi-turn seam it was reviewed as.
//
// Runtime members (Model, Sandbox, Tools, Reviewer, Planner) are swapped
// directly; POLICY members edit the session's RequestedConfig and re-resolve
// through runspec.Resolve, so the mid-session spec is still a Resolve output —
// never a hand-mutated value (PROFILES.md §7.2).

// SetModel swaps the provider used for every SUBSEQUENT turn while keeping the
// conversation, the warm Sandbox, and the open Memory — the /model command:
// switching models mid-chat must not cost you your context. Like Send, it must
// be called from the single goroutine driving the Session (never while a Send
// is in flight).
func (s *Session) SetModel(p llm.Provider) { s.rt.Model = p }

// Model returns the provider the next Send will use.
func (s *Session) Model() llm.Provider { return s.rt.Model }

// SetWorkspace swaps the sandbox/root pair used for every SUBSEQUENT turn while
// keeping the conversation and memory. Interactive front-ends use this for
// per-turn worktree isolation: build a fresh sandbox rooted at the worktree,
// call SetWorkspace, Send once, then restore the normal workspace. Same calling
// contract as SetModel: only from the driving goroutine, never mid-Send.
func (s *Session) SetWorkspace(root string, sb sandbox.Sandbox) {
	s.root = root
	s.rt.Sandbox = sb
	s.rt.VerifySandbox = nil
}

// Workspace returns the root/sandbox pair the next Send will use.
func (s *Session) Workspace() (string, sandbox.Sandbox) { return s.root, s.rt.Sandbox }

// SetMaxIterations changes the per-turn iteration cap for every SUBSEQUENT
// turn (the /set max-iters command) — raising it after a hit_cap turn lets
// "continue" finish a big task without restarting the session. Same calling
// contract as SetModel: only from the driving goroutine, never mid-Send.
// The caller validates; values <= 0 are rejected by Resolve and keep the
// previous spec.
func (s *Session) SetMaxIterations(n int) {
	s.reresolve(func(r *runspec.RequestedConfig) { r.MaxIterations = &n })
}

// MaxIterations returns the cap the next Send will run under.
func (s *Session) MaxIterations() int { return s.spec.Policy().MaxIterations }

// SetMaxTokens changes the per-turn model output cap for every SUBSEQUENT
// turn (the /set max-tokens command). Same calling contract as SetModel.
func (s *Session) SetMaxTokens(n int) {
	s.reresolve(func(r *runspec.RequestedConfig) { r.MaxTokens = &n })
}

// MaxTokens returns the output cap the next Send will run under.
func (s *Session) MaxTokens() int { return s.spec.Policy().MaxTokens }

// SetReasoningEffort changes the reasoning-effort passthrough for every
// SUBSEQUENT turn (the /set effort command; "" = provider default). The
// caller validates the tier name — the wire accepts any string, so a typo
// would otherwise surface as a provider 400 mid-run. Same calling contract as
// SetModel.
func (s *Session) SetReasoningEffort(e string) {
	s.reresolve(func(r *runspec.RequestedConfig) { r.ReasoningEffort = &e })
}

// ReasoningEffort returns the effort the next Send will ask for.
func (s *Session) ReasoningEffort() string { return s.spec.Policy().ReasoningEffort }

// SetVerifyCmd arms (or, with "", disarms) the closing VERIFICATION gate for
// every SUBSEQUENT turn — the /verify command: the user names the success
// command mid-session ("go test ./...") and each finish from then on must pass
// it or the turn ends Unverified (or keeps working, under VerifyContinue).
// The command is runtime-derived session state (like an auto-verify
// derivation), so it lives in the session verifyState, not the spec.
// Same calling contract as SetModel: only from the driving goroutine, never
// mid-Send.
func (s *Session) SetVerifyCmd(cmd string) {
	s.vs.Cmd = cmd
	s.vs.AutoVerifySoft = false
	s.vs.provenance = ""
	if cmd == "" && s.vs.AutoVerify {
		// An explicit /verify off is a user decision, not an invitation for the next
		// turn to auto-arm the gate again.
		s.vs.resolved = true
	}
}

// VerifyCmd returns the verification command the next Send will gate on
// ("" = gate off).
func (s *Session) VerifyCmd() string { return s.vs.Cmd }

// SetPlanner arms (or, with nil, disarms) the PLAN STAGE for every SUBSEQUENT
// turn — the /plan command: an independent planner model explores the tree
// read-only before the solver's first turn and its plan rides into the task.
// Same calling contract as SetModel: only from the driving goroutine, never
// mid-Send.
func (s *Session) SetPlanner(p Planner) { s.rt.Planner = p }

// Planner returns the planner the next Send will open with (nil = stage off).
func (s *Session) Planner() Planner { return s.rt.Planner }

// SetReviewer arms (or, with nil, disarms) the REVIEW GATE for every
// SUBSEQUENT turn — the /review command: an independent reviewer model judges
// each turn's diff after the fence and VerifyCmd pass. Same calling contract
// as SetModel: only from the driving goroutine, never mid-Send.
func (s *Session) SetReviewer(r Reviewer) { s.rt.Reviewer = r }

// Reviewer returns the reviewer the next Send will gate on (nil = gate off).
func (s *Session) Reviewer() Reviewer { return s.rt.Reviewer }

// SetReviewPolicy swaps the review policy for every SUBSEQUENT turn. Callers
// that disarm the reviewer (SetReviewer(nil)) while a required policy is in
// force must downgrade the policy too — validateRun rejects a required policy
// with no Reviewer, which would fail every later Send. Same calling contract
// as SetModel: only from the driving goroutine, never mid-Send.
func (s *Session) SetReviewPolicy(p ReviewPolicy) {
	v := int(p)
	s.reresolve(func(r *runspec.RequestedConfig) { r.ReviewPolicy = &v })
}

// ReviewPolicy returns the policy the next Send will run under.
func (s *Session) ReviewPolicy() ReviewPolicy { return ReviewPolicy(s.spec.Policy().ReviewPolicy) }

// SetTools swaps the toolset used for every SUBSEQUENT turn — the /skills
// picker's seam: toggling a skill rebuilds the `skill` meta-tool (its Level-1
// listing is baked into the tool description) and the whole map is swapped
// here. nil falls back to DefaultTools at loop time, like Runtime.Tools. Same
// calling contract as SetModel: only from the driving goroutine, never
// mid-Send. A skill already loaded into the conversation keeps its body in
// history — SetTools governs what the model can LOAD next, not what it read.
func (s *Session) SetTools(t map[string]Tool) { s.rt.Tools = t }

// Tools returns the toolset the next Send will run with.
func (s *Session) Tools() map[string]Tool { return s.rt.Tools }
