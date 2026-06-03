package agent

import "testing"

// TestVerifySandboxFallback locks the split that lets -session route the model's
// tools through a stateful session while the verification gates run clean: nil
// VerifySandbox falls back to Sandbox (every session-off caller's behavior), and a
// set VerifySandbox is used verbatim.
func TestVerifySandboxFallback(t *testing.T) {
	base := sbWith(t, nil)
	verify := sbWith(t, nil)

	if got := (Config{Sandbox: base}).verifySandbox(); got != base {
		t.Errorf("nil VerifySandbox should fall back to Sandbox; got a different sandbox")
	}
	if got := (Config{Sandbox: base, VerifySandbox: verify}).verifySandbox(); got != verify {
		t.Errorf("set VerifySandbox should be used verbatim; got the base instead")
	}
}
