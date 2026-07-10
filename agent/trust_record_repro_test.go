package agent

import "testing"

// Oracle for PROFILES.md S2b recording: once the binaries select a trust
// profile, the S1-reserved ConfigRecord.TrustProfile field is populated (from
// Config, recording-only), along with the approval-policy identity + hash for
// gated postures. Library callers that set no trust keep the null field —
// the S1 oracle's reserved-null assertion continues to hold for them.
func TestConfigRecordCarriesTrustIdentity(t *testing.T) {
	cfg := Config{Task: "t", TrustProfile: "reviewed-local",
		ApprovalPolicyName: "reviewed-local-readonly-v1", ApprovalPolicyHash: "deadbeef"}
	rec := newConfigRecord(cfg, "sys", nil)
	if rec.TrustProfile == nil || *rec.TrustProfile != "reviewed-local" {
		t.Fatalf("TrustProfile must be recorded when Config.TrustProfile is set; got %v", rec.TrustProfile)
	}
	if rec.ApprovalPolicyName != "reviewed-local-readonly-v1" || rec.ApprovalPolicyHash != "deadbeef" {
		t.Fatalf("approval policy identity+hash must be recorded: %+v", rec)
	}

	// Identity fields are NOT part of the config hash: the hash names the
	// behavior-affecting policy projection, and trust identity is recorded
	// alongside it (same rule as Binary).
	base := newConfigRecord(Config{Task: "t"}, "sys", nil)
	if base.TrustProfile != nil {
		t.Fatalf("no trust configured must keep the reserved null: %v", base.TrustProfile)
	}
	if rec.ConfigSHA256 != base.ConfigSHA256 {
		t.Fatal("trust identity must not move ConfigSHA256 (identity is recorded, not hashed)")
	}
}
