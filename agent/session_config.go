package agent

import "github.com/AccursedGalaxy/driver-os/llm"

// This file extends Session with live reconfiguration for interactive
// front-ends (cmd/cc's /model command). It lives in its own file so the core
// Session (session.go) stays exactly the multi-turn seam it was reviewed as.

// SetModel swaps the provider used for every SUBSEQUENT turn while keeping the
// conversation, the warm Sandbox, and the open Memory — the /model command:
// switching models mid-chat must not cost you your context. Like Send, it must
// be called from the single goroutine driving the Session (never while a Send
// is in flight).
func (s *Session) SetModel(p llm.Provider) { s.cfg.Model = p }

// Model returns the provider the next Send will use.
func (s *Session) Model() llm.Provider { return s.cfg.Model }

// SetMaxIterations changes the per-turn iteration cap for every SUBSEQUENT
// turn (the /set max-iters command) — raising it after a hit_cap turn lets
// "continue" finish a big task without restarting the session. Same calling
// contract as SetModel: only from the driving goroutine, never mid-Send.
// The caller validates; values <= 0 fall back to DefaultMaxIterations.
func (s *Session) SetMaxIterations(n int) { s.cfg.MaxIterations = n }

// MaxIterations returns the cap the next Send will run under.
func (s *Session) MaxIterations() int { return s.cfg.MaxIterations }

// SetMaxTokens changes the per-turn model output cap for every SUBSEQUENT
// turn (the /set max-tokens command). Same calling contract as SetModel.
func (s *Session) SetMaxTokens(n int) { s.cfg.MaxTokens = n }

// MaxTokens returns the output cap the next Send will run under.
func (s *Session) MaxTokens() int { return s.cfg.MaxTokens }
