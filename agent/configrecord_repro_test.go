package agent

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
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
	if cr.Effective.EffectiveProtocol != "tools" {
		t.Fatalf("effective protocol = %q, want tools", cr.Effective.EffectiveProtocol)
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

func TestRunStampsTextConfigRecord(t *testing.T) {
	res, err := runT(context.Background(), Config{
		Model:             &scripted{replies: []string{"answer done"}},
		Sandbox:           sbWith(t, nil),
		RequestedProtocol: "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ConfigRecord == nil {
		t.Fatal("RunResult.ConfigRecord is nil — the text loop must stamp it")
	}
	if got := res.ConfigRecord.Effective.EffectiveProtocol; got != "text" {
		t.Fatalf("effective protocol = %q, want text", got)
	}
	if res.ConfigRecord.RequestedProtocol != "text" || res.ConfigRecord.ProtocolFallbackReason != "" {
		t.Fatalf("explicit text provenance = %+v", res.ConfigRecord)
	}
}

// The hashes are deterministic for identical inputs and sensitive to the
// inputs they name: same (config, prompt, schemas) twice → identical record;
// a policy-field change moves ConfigSHA256 and only ConfigSHA256; a prompt
// change moves PromptSHA256 and only PromptSHA256.
func TestConfigRecordDeterministicAndInputSensitive(t *testing.T) {
	cfg := Config{Task: "t", VerifyCmd: "true", MaxIterations: 3}
	a := newConfigRecordT(cfg, "system prompt", nil)
	b := newConfigRecordT(cfg, "system prompt", nil)
	if a.ConfigSHA256 != b.ConfigSHA256 || a.PromptSHA256 != b.PromptSHA256 || a.ToolSchemaSHA256 != b.ToolSchemaSHA256 {
		t.Fatalf("identical inputs must hash identically:\n%+v\n%+v", a, b)
	}
	cfg2 := cfg
	cfg2.VerifyCmd = "go test ./..."
	c := newConfigRecordT(cfg2, "system prompt", nil)
	if c.ConfigSHA256 == a.ConfigSHA256 {
		t.Fatal("changing a policy field (VerifyCmd) must change ConfigSHA256")
	}
	if c.PromptSHA256 != a.PromptSHA256 {
		t.Fatal("a config-only change must not move PromptSHA256")
	}
	d := newConfigRecordT(cfg, "different system prompt", nil)
	if d.PromptSHA256 == a.PromptSHA256 {
		t.Fatal("changing the system prompt must change PromptSHA256")
	}
	if d.ConfigSHA256 != a.ConfigSHA256 {
		t.Fatal("a prompt-only change must not move ConfigSHA256")
	}
	e := newConfigRecordT(cfg, "system prompt", []llm.Tool{{Name: "read_file"}})
	if e.ToolSchemaSHA256 == a.ToolSchemaSHA256 {
		t.Fatal("changing the tool schemas must change ToolSchemaSHA256")
	}
}

// Invocation surface describes routing rather than behavior, so it is stored
// alongside, rather than within, the effective-config hash.
func TestInvocationSurfaceDoesNotChangeConfigSHA256(t *testing.T) {
	cfg := Config{Task: "t", BinaryIdentity: BinaryIdentityDriver, InvocationSurface: InvocationSurfaceDriverRun}
	run := newConfigRecordT(cfg, "system prompt", nil)
	if run.SchemaVersion != 8 || run.BinaryIdentity != BinaryIdentityDriver || run.InvocationSurface != InvocationSurfaceDriverRun {
		t.Fatalf("v8 identity record = %+v", run)
	}
	cfg.InvocationSurface = InvocationSurfaceDriverAgent
	compat := newConfigRecordT(cfg, "system prompt", nil)
	if run.ConfigSHA256 != compat.ConfigSHA256 {
		t.Fatalf("invocation surface must not affect ConfigSHA256: run=%s agent=%s", run.ConfigSHA256, compat.ConfigSHA256)
	}
	if run.InvocationSurface == compat.InvocationSurface {
		t.Fatal("records must retain their distinct invocation surfaces")
	}
}

func TestConfigRecordProtocolHashing(t *testing.T) {
	cfg := Config{Task: "t", RequestedProtocol: "tools"}
	fallback := newConfigRecordT(cfg, "text prompt", textToolGrammar(nil), "text")
	cfg.ProtocolFallbackReason = "provider lacks tool capability"
	explicit := newConfigRecordT(cfg, "text prompt", textToolGrammar(nil), "text")
	if fallback.ConfigSHA256 != explicit.ConfigSHA256 {
		t.Fatalf("same effective text protocol must hash alike: fallback=%s explicit=%s", fallback.ConfigSHA256, explicit.ConfigSHA256)
	}
	if explicit.ProtocolFallbackReason == "" || explicit.RequestedProtocol != "tools" {
		t.Fatalf("fallback provenance not recorded: %+v", explicit)
	}
	native := newConfigRecordT(cfg, "native prompt", nil, "tools")
	if native.ConfigSHA256 == explicit.ConfigSHA256 {
		t.Fatal("different effective protocols must change ConfigSHA256")
	}
}

// The record survives the transcript write→read round trip, and the
// transcript schema version is bumped for the new field.
func TestTranscriptRoundTripsConfigRecord(t *testing.T) {
	if TranscriptSchemaVersion != "8" {
		t.Fatalf("TranscriptSchemaVersion = %q, want \"7\" (v7 adds the config record)", TranscriptSchemaVersion)
	}
	dir := t.TempDir()
	rec := RunRecord{
		SchemaVersion: TranscriptSchemaVersion,
		ID:            "cfgrec-test",
		Config: &ConfigRecord{
			SchemaVersion:          5,
			PromptSHA256:           "p",
			ToolSchemaSHA256:       "t",
			ConfigSHA256:           "c",
			HarnessCommit:          "abc123",
			HarnessDirty:           true,
			Binary:                 BinaryIdentityDriver,
			BinaryIdentity:         BinaryIdentityDriver,
			InvocationSurface:      InvocationSurfaceDriverRun,
			RequestedProtocol:      "tools",
			ProtocolFallbackReason: "provider lacks tool capability",
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
		g.BinaryIdentity != w.BinaryIdentity || g.InvocationSurface != w.InvocationSurface ||
		g.RequestedProtocol != w.RequestedProtocol || g.ProtocolFallbackReason != w.ProtocolFallbackReason {
		t.Fatalf("config record mutated in round trip:\ngot  %+v\nwant %+v", *g, *w)
	}
}

func TestConfigRecordV8ResolutionProvenance(t *testing.T) {
	cfg := Config{RequiredTrust: "reviewed-local", Canonical: true, FieldProvenance: map[string]string{"max_iters": "profile", "worktree": "trust", "custom": "cli"}}
	a := newConfigRecordT(cfg, "system", nil)
	b := newConfigRecordT(cfg, "system", nil)
	if a.SchemaVersion != 8 || a.RequiredTrust != "reviewed-local" || !a.Canonical {
		t.Fatalf("v8 resolution fields = %+v", a)
	}
	for key, source := range map[string]string{"max_iters": "profile", "worktree": "trust", "custom": "cli", "model_configured": "derived", "finish_tool_configured": "derived"} {
		if a.FieldProvenance[key] != source {
			t.Fatalf("provenance[%q] = %q, want %q", key, a.FieldProvenance[key], source)
		}
	}
	// COMPLETE key set: caller-supplied resolver identifiers UNION the
	// derived-class keys, nothing more, nothing less.
	want := map[string]bool{}
	for k := range cfg.FieldProvenance {
		want[k] = true
	}
	for _, k := range derivedProvenanceKeys {
		want[k] = true
	}
	if len(a.FieldProvenance) != len(want) {
		t.Fatalf("provenance key count = %d, want %d", len(a.FieldProvenance), len(want))
	}
	for k := range want {
		if _, ok := a.FieldProvenance[k]; !ok {
			t.Fatalf("provenance missing key %q", k)
		}
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatalf("v7 record marshal is not deterministic:\n%s\n%s", aj, bj)
	}
}

// A caller-supplied source survives derived stamping (verify_timeout is
// derived unless the CLI set it).
func TestConfigRecordV7CallerSourceWinsOverDerived(t *testing.T) {
	rec := newConfigRecordT(Config{FieldProvenance: map[string]string{"verify_timeout": "cli"}}, "system", nil)
	if rec.FieldProvenance["verify_timeout"] != "cli" {
		t.Fatalf("verify_timeout = %q, want cli", rec.FieldProvenance["verify_timeout"])
	}
}

// Every derivedProvenanceKeys entry must be a real EffectiveConfig JSON tag —
// the list cannot name fields that do not exist (or drift after a rename).
func TestDerivedProvenanceKeysAreEffectiveConfigTags(t *testing.T) {
	tags := map[string]bool{}
	rt := reflect.TypeOf(EffectiveConfig{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			tags[tag] = true
		}
	}
	for _, k := range derivedProvenanceKeys {
		if !tags[k] {
			t.Errorf("derivedProvenanceKeys entry %q is not an EffectiveConfig JSON tag", k)
		}
	}
}

func TestConfigRecordV7DecodesV6WithoutProvenance(t *testing.T) {
	var rec ConfigRecord
	if err := json.Unmarshal([]byte(`{"schema_version":6,"effective":{}}`), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.SchemaVersion != 6 || rec.FieldProvenance != nil || rec.RequiredTrust != "" || rec.Canonical {
		t.Fatalf("v6 decode invented v7 provenance: %+v", rec)
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
func TestConfigRecordDecodesV4(t *testing.T) {
	var rec ConfigRecord
	if err := json.Unmarshal([]byte(`{"schema_version":4,"binary_identity":"driver","invocation_surface":"driver-run","effective":{}}`), &rec); err != nil {
		t.Fatalf("unmarshal v4 config record: %v", err)
	}
	if rec.SchemaVersion != 4 || rec.BinaryIdentity != "driver" || rec.InvocationSurface != "driver-run" || rec.RequestedProtocol != "" || rec.Effective.EffectiveProtocol != "" {
		t.Fatalf("v4 record was not preserved without invented v5 values: %+v", rec)
	}
}
