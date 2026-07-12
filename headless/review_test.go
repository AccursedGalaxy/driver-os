package headless

import (
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/gated"
)

// D5 non-interactive gate policies. These pin the -approve → (approver, policy)
// mapping and the flag/prompt parsing, so the unattended modes can't silently drift
// into asking a human (which would deadlock a script) or auto-allowing too much.

func cmd(line string) sandbox.Command {
	return sandbox.Command{Path: "sh", Args: []string{"-c", line}}
}

func TestReviewGate(t *testing.T) {
	safe := cmd("go test ./...") // DefaultPolicy auto-allows; AskAll does not.
	risky := cmd("rm -rf /")     // never auto-allowed by DefaultPolicy.

	// interactive: an approver IS present (the human decides Ask), policy is the
	// quiet allowlist.
	app, pol := reviewGate("interactive", nil)
	if app == nil {
		t.Error("interactive must have an approver")
	}
	if pol(safe) != gated.Allow {
		t.Error("interactive should auto-allow the safe allowlist")
	}
	if pol(risky) != gated.Ask {
		t.Error("interactive should ask on a risky command")
	}

	// policy: NO approver (Ask fails closed), but the safe allowlist still runs.
	app, pol = reviewGate("policy", nil)
	if app != nil {
		t.Error("policy must have no approver (Ask → blocked)")
	}
	if pol(safe) != gated.Allow {
		t.Error("policy should still auto-allow the safe allowlist")
	}
	if pol(risky) != gated.Ask {
		t.Error("policy keeps DefaultPolicy: risky → Ask (which, approver-less, blocks)")
	}

	// never: AskAll + no approver → EVERY command (even safe) is Ask → blocked.
	app, pol = reviewGate("never", nil)
	if app != nil {
		t.Error("never must have no approver")
	}
	if pol(safe) != gated.Ask {
		t.Error("never must Ask (→ block) even the safe allowlist")
	}
}

func TestValidateReviewFlags(t *testing.T) {
	for _, a := range []string{"interactive", "policy", "never"} {
		if err := validateReviewFlags(a, "keep"); err != nil {
			t.Errorf("approve %q should be valid: %v", a, err)
		}
	}
	for _, r := range []string{"prompt", "commit", "discard", "keep"} {
		if err := validateReviewFlags("policy", r); err != nil {
			t.Errorf("review-action %q should be valid: %v", r, err)
		}
	}
	if validateReviewFlags("yolo", "keep") == nil {
		t.Error("unknown -approve should error")
	}
	if validateReviewFlags("policy", "yolo") == nil {
		t.Error("unknown -review-action should error")
	}
}

func TestPromptDecision(t *testing.T) {
	cases := map[string]string{
		"c": "commit", "commit": "commit", "COMMIT": "commit",
		"d": "discard", "discard": "discard",
		"k": "keep", "keep": "keep", "": "keep", "garbage": "keep", // default is keep.
	}
	for in, want := range cases {
		if got := promptDecision(in); got != want {
			t.Errorf("promptDecision(%q) = %q, want %q", in, got, want)
		}
	}
}
