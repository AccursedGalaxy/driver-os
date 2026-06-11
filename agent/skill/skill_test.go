package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill materializes a skill folder under parent and returns its dir.
func writeSkill(t *testing.T, parent, name, frontmatter, body string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\n" + frontmatter + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "scripts/") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadValid(t *testing.T) {
	dir := writeSkill(t, t.TempDir(), "release-notes",
		// allowed-tools as a LIST (the humanizer-in-the-wild shape) plus the
		// spec's optional fields and an unknown extra — all must be tolerated.
		"name: release-notes\ndescription: |\n  Write release notes.\n  Use when asked for release notes.\nlicense: MIT\nversion: 1.2.0\ncompatibility: any\nmetadata:\n  author: robin\nallowed-tools:\n  - Read\n  - Bash(git:*)\nunknown-field: ignored",
		"# Release notes\n\nDo the thing with ${SKILL_DIR}/scripts/check.sh.",
		map[string]string{
			"scripts/check.sh":  "#!/bin/sh\necho ok\n",
			"references/fmt.md": "the format",
		})

	s, warns, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if s.Name != "release-notes" {
		t.Errorf("Name = %q", s.Name)
	}
	if !strings.Contains(s.Description, "Use when asked") {
		t.Errorf("Description = %q", s.Description)
	}
	if s.Meta["license"] != "MIT" || s.Meta["version"] != "1.2.0" || s.Meta["author"] != "robin" {
		t.Errorf("Meta = %v", s.Meta)
	}
	if want := []string{"references/fmt.md", "scripts/check.sh"}; len(s.Resources) != 2 || s.Resources[0] != want[0] || s.Resources[1] != want[1] {
		t.Errorf("Resources = %v, want %v", s.Resources, want)
	}
	if !strings.Contains(s.Body, "${SKILL_DIR}") {
		t.Errorf("Body lost the substitution token: %q", s.Body)
	}
}

func TestLoadInvalid(t *testing.T) {
	parent := t.TempDir()
	cases := []struct {
		dirName     string
		frontmatter string
		body        string
		wantErr     string
	}{
		{"no-name", "description: d", "body", "`name` is required"},
		{"no-desc", "name: no-desc", "body", "`description` is required"},
		{"bad-chars", "name: Bad_Chars\ndescription: d", "body", "must match"},
		{"mismatch", "name: other-name\ndescription: d", "body", "must equal the folder name"},
		{"empty-body", "name: empty-body\ndescription: d", "", "no body"},
		{"trailing-hyphen", "name: trailing-\ndescription: d", "body", "must match"},
	}
	for _, c := range cases {
		dir := writeSkill(t, parent, c.dirName, c.frontmatter, c.body, nil)
		if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want containing %q", c.dirName, err, c.wantErr)
		}
	}

	// No frontmatter fence at all.
	dir := filepath.Join(parent, "no-fence")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("just markdown\n"), 0o644)
	if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Errorf("no-fence: err = %v", err)
	}

	// Frontmatter never closed.
	dir = filepath.Join(parent, "no-close")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: no-close\ndescription: d\n"), 0o644)
	if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "no closing") {
		t.Errorf("no-close: err = %v", err)
	}
}

func TestLoadWarnings(t *testing.T) {
	parent := t.TempDir()

	// Over-long description: clipped with a warning, not fatal.
	longDesc := strings.Repeat("x", descMax+50)
	dir := writeSkill(t, parent, "long-desc", "name: long-desc\ndescription: "+longDesc, "body", nil)
	s, warns, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len([]rune(s.Description)) > descMax {
		t.Errorf("description not clipped: %d runes", len([]rune(s.Description)))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "description exceeds") {
		t.Errorf("warns = %v", warns)
	}

	// Oversize body: warned, kept.
	bigBody := strings.Repeat("a", bodyWarnRunes+1)
	dir = writeSkill(t, parent, "big-body", "name: big-body\ndescription: d", bigBody, nil)
	if _, warns, err = Load(dir); err != nil || len(warns) != 1 || !strings.Contains(warns[0], "observation backstop") {
		t.Errorf("big-body: err=%v warns=%v", err, warns)
	}

	// A symlink resource is skipped (never staged), with a warning.
	dir = writeSkill(t, parent, "sym-link", "name: sym-link\ndescription: d", "body",
		map[string]string{"references/real.md": "ok"})
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "references", "leak")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	s, warns, err = Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Resources) != 1 || s.Resources[0] != "references/real.md" {
		t.Errorf("Resources = %v, want only references/real.md", s.Resources)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "symlink") {
		t.Errorf("warns = %v", warns)
	}
}

func TestDiscoverPrecedence(t *testing.T) {
	user, project, explicit := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, user, "shared", "name: shared\ndescription: from user", "user body", nil)
	writeSkill(t, project, "shared", "name: shared\ndescription: from project", "project body", nil)
	writeSkill(t, project, "proj-only", "name: proj-only\ndescription: p", "body", nil)
	expDir := writeSkill(t, explicit, "shared", "name: shared\ndescription: from explicit", "explicit body", nil)

	// A broken skill in a layer is skipped with a warning, not fatal.
	writeSkill(t, project, "broken", "description: no name", "body", nil)

	skills, warns := Discover(Sources{UserDir: user, ProjectDir: project, Explicit: []string{expDir}})
	if len(skills) != 2 {
		t.Fatalf("got %d skills: %+v", len(skills), skills)
	}
	if skills[0].Name != "proj-only" || skills[1].Name != "shared" {
		t.Errorf("order = %s, %s", skills[0].Name, skills[1].Name)
	}
	if skills[1].Description != "from explicit" {
		t.Errorf("precedence: shared = %q, want explicit layer", skills[1].Description)
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "overrides") || !strings.Contains(joined, "skipping invalid skill") {
		t.Errorf("warns = %v", warns)
	}

	// Explicit entry as a PARENT dir of skill folders also works.
	skills, _ = Discover(Sources{Explicit: []string{explicit}})
	if len(skills) != 1 || skills[0].Name != "shared" {
		t.Errorf("parent-dir explicit: %+v", skills)
	}

	// A missing explicit entry warns; absent user/project layers are silent.
	_, warns = Discover(Sources{UserDir: filepath.Join(user, "nope"), Explicit: []string{filepath.Join(user, "missing")}})
	if len(warns) != 1 || !strings.Contains(warns[0], "missing") {
		t.Errorf("warns = %v", warns)
	}
}

func TestBuildListing(t *testing.T) {
	mk := func(name, desc string) *Skill { return &Skill{Name: name, Description: desc} }

	// Multi-line descriptions collapse to one line in the text rendering.
	l := buildListing([]*Skill{mk("alpha", "line one\nline two"), mk("beta", "b")}, 0)
	if strings.Contains(l.text, "\n") {
		t.Errorf("text listing must be single-line: %q", l.text)
	}
	if !strings.Contains(l.text, "alpha: line one line two") || !strings.Contains(l.text, "beta: b") {
		t.Errorf("text = %q", l.text)
	}
	if !strings.Contains(l.native, "- alpha: line one line two\n- beta: b") {
		t.Errorf("native = %q", l.native)
	}

	// Per-entry cap.
	l = buildListing([]*Skill{mk("alpha", strings.Repeat("d", entryDescCap+100))}, 0)
	if got := len(l.text); got > entryDescCap+20 {
		t.Errorf("entry not capped: %d chars", got)
	}

	// Overflow drops descriptions from the alphabetically-last skill first,
	// never names. Window of 1200 tokens => 48-char budget: fits aa's entry
	// (36 chars) plus zz name-only, not both descriptions (72).
	l = buildListing([]*Skill{mk("aa", strings.Repeat("x", 30)), mk("zz", strings.Repeat("y", 30))}, 1200)
	if !strings.Contains(l.text, "aa: "+strings.Repeat("x", 30)) {
		t.Errorf("first entry should keep its description: %q", l.text)
	}
	if strings.Contains(l.text, strings.Repeat("y", 30)) || !strings.Contains(l.text, "zz") {
		t.Errorf("last entry should lose its description but keep its name: %q", l.text)
	}
}
