package agent

import "testing"

// Oracle for PROFILES.md S3 recording: the S1-reserved ExecProfile field is
// populated once a surface selects an execution profile, together with the
// profile content hash and the CLI-override provenance list. Identity fields
// stay out of the config hash (same rule as Binary/TrustProfile).
func TestConfigRecordCarriesExecProfileIdentity(t *testing.T) {
	cfg := Config{Task: "t",
		ExecProfileName: "coding-v1", ExecProfileHash: "abc123",
		CLIOverrides: []string{"max-iters", "verify-cmd"}}
	rec := newConfigRecordT(cfg, "sys", nil)
	if rec.ExecProfile == nil || *rec.ExecProfile != "coding-v1" {
		t.Fatalf("ExecProfile must be recorded when Config.ExecProfileName is set; got %v", rec.ExecProfile)
	}
	if rec.ExecProfileHash != "abc123" {
		t.Fatalf("profile content hash must be recorded: %+v", rec)
	}
	if len(rec.CLIOverrides) != 2 || rec.CLIOverrides[0] != "max-iters" {
		t.Fatalf("CLI override provenance must be recorded verbatim: %+v", rec.CLIOverrides)
	}

	base := newConfigRecordT(Config{Task: "t"}, "sys", nil)
	if base.ExecProfile != nil || base.ExecProfileHash != "" || base.CLIOverrides != nil {
		t.Fatalf("no profile configured keeps the reserved nulls: %+v", base)
	}
	if rec.ConfigSHA256 != base.ConfigSHA256 {
		t.Fatal("profile identity must not move ConfigSHA256 (identity is recorded, not hashed)")
	}
}
