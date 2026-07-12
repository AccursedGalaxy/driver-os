// Package cli holds the setup logic shared by the driver-os command front-ends
// (cmd/agent, cmd/chat, …): resolving the LLM provider from env+flags, building
// the sandbox backend, and validating the safety-relevant flag combinations. It
// is the ONE place these decisions live, so every front-end picks a backend and
// enforces isolation identically. Loop/termination policy stays in package agent;
// this is purely the wiring a main() needs before it can call the loop.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/AccursedGalaxy/driver-os/llm"
	"github.com/AccursedGalaxy/driver-os/provider/anthropic"
	"github.com/AccursedGalaxy/driver-os/provider/mockmodel"
	"github.com/AccursedGalaxy/driver-os/provider/openaicompat"
	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/docker"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

// ErrNoProviderKey is the auto-mode "no key anywhere" sentinel, so a front-end can
// label that specific setup failure distinctly (vs a named-provider key error).
var ErrNoProviderKey = errors.New("no provider key found; set OPENROUTER_API_KEY, ANTHROPIC_API_KEY, X_AI_API_KEY, or OPENAI_API_KEY in .env (or pass -provider=ollama for a keyless local backend)")

// PickProvider resolves the LLM backend from the provider and model selectors
// (D6). An empty provider auto-infers from whichever key is present (the
// historical behavior); a NAMED provider whose key is absent is a setup error
// rather than a silent fallback, so a script gets the backend it asked for or a
// clear refusal. model overrides the provider's *_MODEL env default; the flag
// always wins.
func PickProvider(provider, model string) (llm.Provider, error) {
	switch provider {
	case "", "auto":
		switch {
		case os.Getenv("OPENROUTER_API_KEY") != "":
			return openaicompat.OpenRouter(modelOr(model, "OPENROUTER_MODEL", "openai/gpt-4o-mini")).WithPromptCache(), nil
		case os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("CLAUDE_API_KEY") != "":
			return anthropic.Claude(modelOr(model, "ANTHROPIC_MODEL", "claude-haiku-4-5")), nil
		case os.Getenv("X_AI_API_KEY") != "":
			return openaicompat.XAI(modelOr(model, "XAI_MODEL", "grok-3-mini")), nil
		case os.Getenv("OPENAI_API_KEY") != "":
			return openaicompat.OpenAI(modelOr(model, "OPENAI_MODEL", "gpt-4o-mini")), nil
		default:
			return nil, ErrNoProviderKey
		}
	case "openrouter":
		return needKey("OPENROUTER_API_KEY", provider, func() llm.Provider {
			return openaicompat.OpenRouter(modelOr(model, "OPENROUTER_MODEL", "openai/gpt-4o-mini")).WithPromptCache()
		})
	case "anthropic":
		// CLAUDE_API_KEY is an accepted alias (see anthropic.Claude).
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("CLAUDE_API_KEY") == "" {
			return nil, fmt.Errorf("-provider=%s but ANTHROPIC_API_KEY is not set", provider)
		}
		return anthropic.Claude(modelOr(model, "ANTHROPIC_MODEL", "claude-haiku-4-5")), nil
	case "xai":
		return needKey("X_AI_API_KEY", provider, func() llm.Provider {
			return openaicompat.XAI(modelOr(model, "XAI_MODEL", "grok-3-mini"))
		})
	case "openai":
		return needKey("OPENAI_API_KEY", provider, func() llm.Provider {
			return openaicompat.OpenAI(modelOr(model, "OPENAI_MODEL", "gpt-4o-mini"))
		})
	case "ollama":
		// Keyless local backend; model (or OLLAMA_MODEL) names the local model.
		return openaicompat.Ollama(modelOr(model, "OLLAMA_MODEL", "llama3.2")), nil
	case "mock":
		// Keyless local scripted backend for deterministic TUI runs.
		return mockmodel.New(model)
	default:
		return nil, fmt.Errorf("unknown -provider %q (want auto, openrouter, anthropic, xai, openai, ollama, or mock)", provider)
	}
}

// SplitModelRef splits an optional "provider:model" reference — the syntax
// that lets any ROLE (solver /model, planner /plan, reviewer /review) sit on a
// different backend than the session default, e.g. "anthropic:claude-fable-5"
// while the solver runs on OpenRouter. Only a prefix naming a KNOWN provider
// kind splits: OpenRouter model ids legitimately contain ":" (e.g.
// "deepseek/deepseek-chat:free"), so an unrecognized prefix leaves the ref
// intact as a bare model id (provider "" = caller's default).
func SplitModelRef(ref string) (provider, model string) {
	if i := strings.IndexByte(ref, ':'); i > 0 {
		switch p := ref[:i]; p {
		case "openrouter", "anthropic", "xai", "openai", "ollama", "mock":
			return p, ref[i+1:]
		}
	}
	return "", ref
}

// needKey builds a provider only when its API key is present, else returns the
// missing-key setup error naming both the provider and the env var to set.
func needKey(env, provider string, build func() llm.Provider) (llm.Provider, error) {
	if os.Getenv(env) == "" {
		return nil, fmt.Errorf("-provider=%s but %s is not set", provider, env)
	}
	return build(), nil
}

// modelOr resolves the model id: the flag wins, else the provider's *_MODEL env
// var, else the built-in default.
func modelOr(flagModel, envKey, fallback string) string {
	if flagModel != "" {
		return flagModel
	}
	return envOr(envKey, fallback)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// BuildSandbox opens the isolation backend every effect flows through (see
// ../../sandbox + ../../docs/specs/SANDBOX.md). kind picks the backend: "local"
// (IsolationNone — host subprocess, TRUSTED code only) or "docker" (a
// resource-capped container; runtime="runsc" selects gVisor / kernel isolation,
// network toggles egress). The caller is responsible for Close.
// BuildSandbox opens a sandbox with the full ambient environment for local runs.
func BuildSandbox(ctx context.Context, kind, dir, runtime string, network bool) (sandbox.Sandbox, error) {
	return BuildSandboxWithEnvAllowlist(ctx, kind, dir, runtime, network, nil)
}

// BuildSandboxWithEnvAllowlist opens the isolation backend and, for a local
// sandbox, optionally limits inherited command environment variables by name.
func BuildSandboxWithEnvAllowlist(ctx context.Context, kind, dir, runtime string, network bool, envAllowlist []string) (sandbox.Sandbox, error) {
	switch kind {
	case "local":
		return local.NewWithOptions(dir, local.Options{EnvAllowlist: envAllowlist})
	case "docker":
		// runtime=runc is the daemon default (empty Runtime => process isolation);
		// runsc selects gVisor (kernel isolation) — the one-flag graduation.
		rt := ""
		if runtime == "runsc" {
			rt = "runsc"
		}
		return docker.New(ctx, dir, docker.Options{Runtime: rt, Network: network})
	default:
		return nil, fmt.Errorf("unknown -sandbox %q (want 'local' or 'docker')", kind)
	}
}

// ValidateSandboxFlags rejects nonsensical or unsafe flag COMBINATIONS up front,
// so a typo can't silently weaken isolation and an unsafe request is refused
// before any container starts. The in-loop MinIsolation gate is still the
// authoritative safety boundary; this is the fail-fast layer in front of it.
func ValidateSandboxFlags(kind, runtime string, network, untrusted bool) error {
	if kind != "local" && kind != "docker" {
		return fmt.Errorf("unknown -sandbox %q (want 'local' or 'docker')", kind)
	}
	// An unknown -runtime must ERROR, never silently fall back to runc — a typo
	// like '-runtime=runcc' could otherwise drop you to process isolation while
	// you believed you had gVisor.
	if runtime != "runc" && runtime != "runsc" {
		return fmt.Errorf("unknown -runtime %q (want 'runc' or 'runsc')", runtime)
	}
	if kind == "local" && (runtime != "runc" || network) {
		return fmt.Errorf("-runtime/-network apply only to -sandbox=docker; drop them or pass -sandbox=docker")
	}
	if untrusted {
		// Hostile code requires gVisor AND no network egress. Enforce both here so we
		// never even start a too-weak or networked container for an untrusted task.
		if kind != "docker" || runtime != "runsc" {
			return fmt.Errorf("-untrusted requires -sandbox=docker -runtime=runsc (kernel isolation); refusing on a weaker boundary")
		}
		if network {
			return fmt.Errorf("-untrusted forbids -network: hostile code must not have network egress")
		}
	}
	return nil
}

// TrustLabel renders how much to trust a given isolation level in a startup log,
// so a plain shared-kernel container is never mislabeled as safe for hostile code.
func TrustLabel(iso sandbox.Isolation) string {
	switch {
	case iso >= sandbox.IsolationKernel:
		return "kernel-isolated (gVisor) — safe for untrusted code"
	case iso >= sandbox.IsolationProcess:
		return "shared-kernel container — NOT sufficient for -untrusted code"
	default:
		return "trusted code only"
	}
}

// SplitList splits a comma-separated flag value, dropping empty entries.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
