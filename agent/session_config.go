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
