package agent

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/AccursedGalaxy/driver-os/llm"
)

// Oracle for PROFILES.md S1 (recording slice, review-triage R2): every native
// run stamps a ConfigRecord — canonical effective-config serialization +
// hashes (prompt, tool-schema, config), harness build identity, and RESERVED
// null profile fields — with zero behavior change. Full design:
// docs/specs/PROFILES.md §4.

// The native loop must fill RunResult.ConfigRecord on every run.
func TestRunNativeStampsConfigRecord(t *testing.T) {
	res, _, _ := runNative(t, nil, [][]llm.ContentPart{{llm.Text("done already")}})
	cr := res.ConfigRecord
	if cr == nil {
		t.Fatal("RunResult.ConfigRecord is nil — the native loop must stamp it on every run")
	}
	if cr.PromptSHA256 == "" || cr.ToolSchemaSHA256 == "" || cr.ConfigSHA256 == "" {
		t.Fatalf("hashes must all be set: prompt=%q toolschema=%q config=%q",
			cr.PromptSHA256, cr.ToolSchemaSHA256, cr.ConfigSHA256)
	}
	if cr.TrustProfile != nil || cr.ExecProfile != nil {
		t.Fatalf("profile identity fields are RESERVED and must stay null until S2/S3; got trust=%v exec=%v",
			cr.TrustProfile, cr.ExecProfile)
	}
}

// The hashes are deterministic for identical inputs and sensitive to the
// inputs they name: same (config, prompt, schemas) twice → identical record;
// a policy-field change moves ConfigSHA256 and only ConfigSHA256; a prompt
// change moves PromptSHA256 and only PromptSHA256.
func TestConfigRecordDeterministicAndInputSensitive(t *testing.T) {
	cfg := Config{Task: "t", VerifyCmd: "true", MaxIterations: 3}
	a := newConfigRecord(cfg, "system prompt", nil)
	b := newConfigRecord(cfg, "system prompt", nil)
	if a.ConfigSHA256 != b.ConfigSHA256 || a.PromptSHA256 != b.PromptSHA256 || a.ToolSchemaSHA256 != b.ToolSchemaSHA256 {
		t.Fatalf("identical inputs must hash identically:\n%+v\n%+v", a, b)
	}
	cfg2 := cfg
	cfg2.VerifyCmd = "go test ./..."
	c := newConfigRecord(cfg2, "system prompt", nil)
	if c.ConfigSHA256 == a.ConfigSHA256 {
		t.Fatal("changing a policy field (VerifyCmd) must change ConfigSHA256")
	}
	if c.PromptSHA256 != a.PromptSHA256 {
		t.Fatal("a config-only change must not move PromptSHA256")
	}
	d := newConfigRecord(cfg, "different system prompt", nil)
	if d.PromptSHA256 == a.PromptSHA256 {
		t.Fatal("changing the system prompt must change PromptSHA256")
	}
	if d.ConfigSHA256 != a.ConfigSHA256 {
		t.Fatal("a prompt-only change must not move ConfigSHA256")
	}
	e := newConfigRecord(cfg, "system prompt", []llm.Tool{{Name: "read_file"}})
	if e.ToolSchemaSHA256 == a.ToolSchemaSHA256 {
		t.Fatal("changing the tool schemas must change ToolSchemaSHA256")
	}
}

// Invocation surface describes routing rather than behavior, so it is stored
// alongside, rather than within, the effective-config hash.
func TestInvocationSurfaceDoesNotChangeConfigSHA256(t *testing.T) {
	cfg := Config{Task: "t", BinaryIdentity: BinaryIdentityDriver, InvocationSurface: InvocationSurfaceDriverRun}
	run := newConfigRecord(cfg, "system prompt", nil)
	if run.SchemaVersion != 4 || run.BinaryIdentity != BinaryIdentityDriver || run.InvocationSurface != InvocationSurfaceDriverRun {
		t.Fatalf("v4 identity record = %+v", run)
	}
	cfg.InvocationSurface = InvocationSurfaceDriverAgent
	compat := newConfigRecord(cfg, "system prompt", nil)
	if run.ConfigSHA256 != compat.ConfigSHA256 {
		t.Fatalf("invocation surface must not affect ConfigSHA256: run=%s agent=%s", run.ConfigSHA256, compat.ConfigSHA256)
	}
	if run.InvocationSurface == compat.InvocationSurface {
		t.Fatal("records must retain their distinct invocation surfaces")
	}
}

// The record survives the transcript write→read round trip, and the
// transcript schema version is bumped for the new field.
func TestTranscriptRoundTripsConfigRecord(t *testing.T) {
	if TranscriptSchemaVersion != "7" {
		t.Fatalf("TranscriptSchemaVersion = %q, want \"7\" (v7 adds the config record)", TranscriptSchemaVersion)
	}
	dir := t.TempDir()
	rec := RunRecord{
		SchemaVersion: TranscriptSchemaVersion,
		ID:            "cfgrec-test",
		Config: &ConfigRecord{
			SchemaVersion:     4,
			PromptSHA256:      "p",
			ToolSchemaSHA256:  "t",
			ConfigSHA256:      "c",
			HarnessCommit:     "abc123",
			HarnessDirty:      true,
			Binary:            BinaryIdentityDriver,
			BinaryIdentity:    BinaryIdentityDriver,
			InvocationSurface: InvocationSurfaceDriverRun,
		},
	}
	path, err := WriteTranscript(dir, rec)
	if err != nil {
		t.Fatalf("WriteTranscript: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got RunRecord
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("transcript is not valid JSON: %v", err)
	}
	if got.Config == nil {
		t.Fatal("config record lost in round trip")
	}
	g, w := got.Config, rec.Config
	if g.PromptSHA256 != w.PromptSHA256 || g.ToolSchemaSHA256 != w.ToolSchemaSHA256 ||
		g.ConfigSHA256 != w.ConfigSHA256 || g.HarnessCommit != w.HarnessCommit ||
		g.HarnessDirty != w.HarnessDirty || g.Binary != w.Binary ||
		g.BinaryIdentity != w.BinaryIdentity || g.InvocationSurface != w.InvocationSurface {
		t.Fatalf("config record mutated in round trip:\ngot  %+v\nwant %+v", *g, *w)
	}
}

func TestConfigRecordDecodesLegacyBinary(t *testing.T) {
	var rec ConfigRecord
	if err := json.Unmarshal([]byte(`{"schema_version":3,"binary":"cmd/agent"}`), &rec); err != nil {
		t.Fatalf("unmarshal legacy config record: %v", err)
	}
	if rec.Binary != "cmd/agent" || rec.BinaryIdentity != "" || rec.InvocationSurface != "" {
		t.Fatalf("legacy binary was not preserved without invented identities: %+v", rec)
	}
}
