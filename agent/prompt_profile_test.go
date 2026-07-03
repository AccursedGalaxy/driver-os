package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// PROMPT-SKILLS slice 2: PromptProfile selects the native loop's base prompt.
// The zero value must stay byte-identical to the historical prompt (every
// existing caller is an implicit "legacy" arm), and an unknown profile must
// refuse to start — a typo that silently ran the wrong arm would corrupt a
// paid A/B.

// promptFor resolves a fixed profile the way RunNative does (no model routing).
func promptFor(profile string) (string, error) {
	s, _, err := resolveSystemPrompt(Config{PromptProfile: profile})
	return s, err
}

func TestSystemPromptForProfiles(t *testing.T) {
	legacy, err := promptFor("")
	if err != nil {
		t.Fatalf("empty profile: %v", err)
	}
	if legacy != nativeSystemPrompt() {
		t.Errorf("empty profile is not the historical prompt")
	}
	named, err := promptFor("legacy")
	if err != nil || named != legacy {
		t.Errorf("'legacy' should alias the empty profile (err=%v)", err)
	}

	structured, err := promptFor("structured")
	if err != nil {
		t.Fatalf("structured profile: %v", err)
	}
	// Content smoke checks: the council-calibrated rules must actually be there
	// (docs/specs/PROMPT-SKILLS.md), and the loop contract must survive.
	for _, want := range []string{
		"Working rules:",
		"NEVER weaken, delete, or special-case a test", // test integrity: blocks reward hacking...
		"is normal work",        // ...without blocking test maintenance.
		"explanation-only task", // verification escape hatch.
		"NEVER assume a library is available",
		"DO NOT call a tool", // the finish contract shared with legacy.
	} {
		if !strings.Contains(structured, want) {
			t.Errorf("structured prompt is missing %q", want)
		}
	}
	if structured == legacy {
		t.Errorf("structured prompt did not change from legacy")
	}

	if _, err := promptFor("structrued"); err == nil {
		t.Fatalf("typo profile did not error")
	} else if !strings.Contains(err.Error(), "structrued") {
		t.Errorf("error should name the bad profile, got: %v", err)
	}
}

// An unknown profile must abort BEFORE the first model call (the plan stage
// bills first) — not fall back, not run mislabeled.
func TestRunNativeUnknownPromptProfileRefusesBeforeModelCall(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("should never run")}}}
	sb := sbWith(t, nil)
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", PromptProfile: "nope"})
	if err == nil {
		t.Fatalf("RunNative accepted an unknown PromptProfile (res=%+v)", res)
	}
	if len(ns.calls) != 0 {
		t.Errorf("provider was called %d time(s) despite the invalid profile", len(ns.calls))
	}
}

// The selected profile must actually ride on the request's system prompt.
func TestRunNativeStructuredProfileRidesSystemPrompt(t *testing.T) {
	ns := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
	sb := sbWith(t, nil)
	res, err := RunNative(context.Background(), Config{Model: ns, Sandbox: sb, Task: "t", PromptProfile: "structured"})
	if err != nil {
		t.Fatalf("RunNative: %v", err)
	}
	if res.Outcome != Answered {
		t.Fatalf("Outcome = %q (%s)", res.Outcome, res.Reason)
	}
	if len(ns.calls) == 0 || !strings.Contains(ns.calls[0].System, "Working rules:") {
		t.Errorf("first request's System does not carry the structured prompt")
	}
	// And the default arm must NOT carry it (the A/B differs in this field alone).
	ns2 := &nativeScript{turns: [][]llm.ContentPart{{llm.Text("done")}}}
	if _, err := RunNative(context.Background(), Config{Model: ns2, Sandbox: sbWith(t, nil), Task: "t"}); err != nil {
		t.Fatalf("legacy run: %v", err)
	}
	if len(ns2.calls) == 0 || strings.Contains(ns2.calls[0].System, "Working rules:") {
		t.Errorf("default profile unexpectedly carries the structured prompt")
	}
}
