package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
	"github.com/AccursedGalaxy/driver-os/sandbox/gated"
	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

func TestToolLoadStageAndRepeat(t *testing.T) {
	ctx := context.Background()
	skillDir := writeSkill(t, t.TempDir(), "greeter",
		"name: greeter\ndescription: Greet things. Use when greeting.",
		"# Greeting procedure\n\nRun ${SKILL_DIR}/scripts/greet.sh and read ${CLAUDE_SKILL_DIR}/references/notes.md.",
		map[string]string{
			"scripts/greet.sh":    "#!/bin/sh\necho greetings\n",
			"references/notes.md": "be nice",
		})
	s, _, err := Load(skillDir)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	tool := Tool([]*Skill{s}, sb, 0)
	if tool.Name != "skill" {
		t.Fatalf("Name = %q", tool.Name)
	}
	if !strings.Contains(tool.Desc, "greeter: Greet things.") {
		t.Errorf("Desc missing listing: %q", tool.Desc)
	}
	if strings.Contains(tool.Desc, "\n") {
		t.Errorf("text Desc must be single-line")
	}
	if !strings.Contains(tool.NativeDesc, "- greeter: Greet things.") {
		t.Errorf("NativeDesc missing listing: %q", tool.NativeDesc)
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("Schema is not valid JSON: %v", err)
	}

	// First load: header + substituted body, resources staged with exec bits.
	obs, err := tool.Run(ctx, "greeter")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{
		`SKILL "greeter" loaded`,
		".skills/greeter/scripts/greet.sh",
		"Run .skills/greeter/scripts/greet.sh",        // ${SKILL_DIR} substituted
		"read .skills/greeter/references/notes.md",    // ${CLAUDE_SKILL_DIR} substituted
		"Bundled files staged under .skills/greeter/", // header names the staging dir
	} {
		if !strings.Contains(obs, want) {
			t.Errorf("observation missing %q:\n%s", want, obs)
		}
	}
	info, err := os.Stat(filepath.Join(root, ".skills", "greeter", "scripts", "greet.sh"))
	if err != nil {
		t.Fatalf("staged script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("staged script lost its exec bit: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(root, ".skills", "greeter", "references", "notes.md")); err != nil {
		t.Errorf("staged reference: %v", err)
	}

	// Repeat load: cheap reminder, not the body again.
	obs2, err := tool.Run(ctx, "greeter")
	if err != nil {
		t.Fatalf("repeat Run: %v", err)
	}
	if !strings.Contains(obs2, "already loaded") || strings.Contains(obs2, "Greeting procedure") {
		t.Errorf("repeat load should be a reminder, got:\n%s", obs2)
	}

	// Unknown skill: a teaching error naming the valid set.
	if _, err := tool.Run(ctx, "nonesuch"); err == nil || !strings.Contains(err.Error(), "greeter") {
		t.Errorf("unknown skill err = %v", err)
	}

	// Forgiving arg normalization: quotes and case.
	if obs, err := tool.Run(ctx, ` "Greeter" `); err != nil || !strings.Contains(obs, "already loaded") {
		t.Errorf("normalized name: obs=%q err=%v", obs, err)
	}
}

func TestToolRunJSON(t *testing.T) {
	ctx := context.Background()
	skillDir := writeSkill(t, t.TempDir(), "plain",
		"name: plain\ndescription: d", "the body", nil)
	s, _, err := Load(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	tool := Tool([]*Skill{s}, sb, 0)
	obs, err := tool.RunJSON(ctx, json.RawMessage(`{"name":"plain"}`))
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if !strings.Contains(obs, "the body") {
		t.Errorf("obs = %q", obs)
	}
	// No resources => no staging dir, no staging note.
	if strings.Contains(obs, "Bundled files") {
		t.Errorf("resource note on a resource-free skill:\n%s", obs)
	}
	if _, err := tool.RunJSON(ctx, json.RawMessage(`{`)); err == nil {
		t.Error("malformed JSON should error")
	}
}

// LoadForUser is the /skills force-load path: same staging and body as the
// tool, a header that says the USER picked it, and no dependence on the
// tool's dedup state or the enabled listing.
func TestLoadForUser(t *testing.T) {
	ctx := context.Background()
	skillDir := writeSkill(t, t.TempDir(), "greeter",
		"name: greeter\ndescription: Greet things.",
		"# Greeting procedure\n\nRun ${SKILL_DIR}/scripts/greet.sh.",
		map[string]string{"scripts/greet.sh": "#!/bin/sh\necho hi\n"})
	s, _, err := Load(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	text, err := LoadForUser(ctx, sb, s)
	if err != nil {
		t.Fatalf("LoadForUser: %v", err)
	}
	for _, want := range []string{
		`SKILL "greeter" loaded at the user's request`,
		"Run .skills/greeter/scripts/greet.sh", // ${SKILL_DIR} substituted
		"Bundled files staged under .skills/greeter/",
		"Greeting procedure",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".skills", "greeter", "scripts", "greet.sh")); err != nil {
		t.Errorf("staged script: %v", err)
	}
}

func TestExcludeStaging(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing exclude content must be preserved, not clobbered.
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := writeSkill(t, t.TempDir(), "with-res",
		"name: with-res\ndescription: d", "body", map[string]string{"references/a.md": "a"})
	s, _, err := Load(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	if _, err := Tool([]*Skill{s}, sb, 0).Run(ctx, "with-res"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "existing\n") || !strings.Contains(got, ".skills/\n") {
		t.Errorf("exclude = %q", got)
	}

	// Idempotent: a second skill load must not duplicate the line.
	skillDir2 := writeSkill(t, t.TempDir(), "with-res2",
		"name: with-res2\ndescription: d", "body", map[string]string{"references/b.md": "b"})
	s2, _, _ := Load(skillDir2)
	if _, err := Tool([]*Skill{s2}, sb, 0).Run(ctx, "with-res2"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if strings.Count(string(data), ".skills/") != 1 {
		t.Errorf("exclude line duplicated: %q", string(data))
	}
}

// TestStageUnderDenyAllGate pins the exec-free staging contract: skill staging
// uses only native sandbox file ops (MakeDirAll/WriteFile), so it must succeed
// even under a gate that denies EVERY exec — the shape gated trust profiles
// present to harness housekeeping.
func TestStageUnderDenyAllGate(t *testing.T) {
	ctx := context.Background()
	skillDir := writeSkill(t, t.TempDir(), "gatedskill",
		"name: gatedskill\ndescription: Gated. Use when gated.",
		"# Body\n\nUse ${SKILL_DIR}/scripts/x.sh.",
		map[string]string{"scripts/x.sh": "#!/bin/sh\necho hi\n"})
	s, _, err := Load(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	gate := gated.New(inner, nil, func(sandbox.Command) gated.Verdict { return gated.Deny })
	if _, err := LoadForUser(ctx, gate, s); err != nil {
		t.Fatalf("staging must not require exec under a deny-all gate: %v", err)
	}
	if _, err := gate.ReadFile(ctx, ".skills/gatedskill/scripts/x.sh"); err != nil {
		t.Fatalf("staged resource missing: %v", err)
	}
}
