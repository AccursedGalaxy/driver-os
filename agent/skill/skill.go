// Package skill implements Agent Skills for the harness: folders of
// instructions (SKILL.md) plus optional bundled resources, loaded by the agent
// ON DEMAND instead of stuffed into the system prompt — progressive disclosure
// (see ../../SKILLS.md). The format is the open Agent Skills spec
// (https://agentskills.io/specification) verbatim, so skills written for
// Claude Code / Codex / Gemini CLI load here unmodified.
//
// The package is deliberately an orchestration-layer feature: the loop never
// changes. Discovery happens before agent.Run, and the whole runtime surface
// is ONE extra tool (Tool) whose description carries the Level-1 listing.
package skill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// nameMax / descMax are the open spec's hard field bounds.
	nameMax = 64
	descMax = 1024

	// bodyWarnRunes triggers a load-time warning: the loop's observation
	// backstop clips at 12,000 runes (agent.observationCap), so a body over
	// ~10k risks arriving clipped — a short skill beats a truncated one.
	bodyWarnRunes = 10000

	// maxSkillMD bounds how much of a SKILL.md we read at all (P4 — a fence
	// against a pathological file, not a policy).
	maxSkillMD = 1 << 20 // 1 MiB

	// Resource staging caps (P4): a single bundled file over fileCap is
	// skipped, and a skill's resources stop counting once totalCap is reached.
	fileCap  = 1 << 20  // 1 MiB per file
	totalCap = 16 << 20 // 16 MiB per skill
)

// nameRe is the spec's skill-name grammar: lowercase alphanumerics and single
// hyphens, no leading/trailing hyphen. It structurally forbids "/", "." and
// whitespace, which is also what makes a validated name safe to embed in the
// .skills/<name>/ staging path.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is one parsed, validated skill folder.
type Skill struct {
	Name        string            // == the folder's base name (spec requirement)
	Description string            // what it does AND when to use it — the sole trigger signal
	Dir         string            // absolute host path of the skill folder
	Body        string            // SKILL.md content below the frontmatter
	Meta        map[string]string // passthrough: license, compatibility, metadata.*
	Resources   []string          // slash-relative paths of bundled files, sorted
}

// frontmatter is the spec schema plus tolerance for fields seen in the wild
// (version, allowed-tools as either a string or a list). Unknown fields are
// ignored, not errors — forward compatibility is the point of a spec.
type frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Version       string            `yaml:"version"`
	Metadata      map[string]string `yaml:"metadata"`
	// AllowedTools is parsed so a conformant file round-trips, but v1 ignores
	// it: every major harness treats it as pre-approval (not restriction), and
	// we have no per-tool permission layer to pre-approve against. SKILLS.md §D6.
	AllowedTools yaml.Node `yaml:"allowed-tools"`
}

// Load parses and validates one skill folder. A spec-invalid folder returns a
// nil Skill and a reason — callers warn and SKIP, they never fail the run
// (Gemini CLI semantics: a broken skill is a degraded mode, not a crash).
// warnings carries non-fatal findings (oversized body, skipped resources).
func Load(dir string) (s *Skill, warnings []string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving %q: %w", dir, err)
	}
	raw, err := readCapped(filepath.Join(abs, "SKILL.md"), maxSkillMD)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: reading SKILL.md: %w", abs, err)
	}

	fmRaw, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", abs, err)
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid frontmatter YAML: %v", abs, err)
	}

	name := strings.TrimSpace(fm.Name)
	switch {
	case name == "":
		return nil, nil, fmt.Errorf("%s: frontmatter `name` is required", abs)
	case len(name) > nameMax:
		return nil, nil, fmt.Errorf("%s: `name` exceeds %d chars", abs, nameMax)
	case !nameRe.MatchString(name):
		return nil, nil, fmt.Errorf("%s: `name` %q must match %s (lowercase, digits, single hyphens)", abs, name, nameRe)
	case name != filepath.Base(abs):
		return nil, nil, fmt.Errorf("%s: `name` %q must equal the folder name %q", abs, name, filepath.Base(abs))
	}

	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		return nil, nil, fmt.Errorf("%s: frontmatter `description` is required", abs)
	}
	if utf8.RuneCountInString(desc) > descMax {
		// Over-length is clipped, not fatal: the description still triggers,
		// and the listing has its own per-entry cap anyway.
		desc = clipRunes(desc, descMax)
		warnings = append(warnings, fmt.Sprintf("skill %q: description exceeds %d chars — clipped in the listing", name, descMax))
	}

	body = strings.TrimSpace(body)
	if body == "" {
		return nil, nil, fmt.Errorf("%s: SKILL.md has no body below the frontmatter", abs)
	}
	if n := utf8.RuneCountInString(body); n > bodyWarnRunes {
		warnings = append(warnings, fmt.Sprintf(
			"skill %q: body is %d runes — over ~%d it risks arriving clipped by the loop's observation backstop; split detail into references/ files", name, n, bodyWarnRunes))
	}

	meta := map[string]string{}
	for k, v := range fm.Metadata {
		meta[k] = v
	}
	if fm.License != "" {
		meta["license"] = fm.License
	}
	if fm.Compatibility != "" {
		meta["compatibility"] = fm.Compatibility
	}
	if fm.Version != "" {
		meta["version"] = fm.Version
	}

	res, resWarns := listResources(abs, name)
	warnings = append(warnings, resWarns...)

	return &Skill{
		Name:        name,
		Description: desc,
		Dir:         abs,
		Body:        body,
		Meta:        meta,
		Resources:   res,
	}, warnings, nil
}

// splitFrontmatter separates the YAML frontmatter from the Markdown body. The
// spec (and Gemini's strict reading of it) requires the file to BEGIN with
// "---" — anything before it disqualifies the skill.
func splitFrontmatter(s string) (fm, body string, err error) {
	// Tolerate a UTF-8 BOM and CRLF line endings, nothing else.
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", "", fmt.Errorf("SKILL.md must start with a `---` frontmatter block")
	}
	rest := s[strings.Index(s, "\n")+1:]
	for _, sep := range []string{"\n---\n", "\n---\r\n"} {
		if i := strings.Index(rest, sep); i >= 0 {
			return rest[:i], rest[i+len(sep):], nil
		}
	}
	// A file ending exactly at the closing fence has no body — caught by the
	// empty-body check in Load, but recognise the fence here too.
	if strings.HasSuffix(rest, "\n---") {
		return strings.TrimSuffix(rest, "\n---"), "", nil
	}
	return "", "", fmt.Errorf("SKILL.md frontmatter has no closing `---`")
}

// listResources walks the skill folder collecting bundled files (everything
// except SKILL.md), bounded by the staging caps. Only REGULAR files count:
// symlinks are skipped — a skill must not be able to alias host files outside
// its folder into the sandbox (the staging copy would dutifully exfiltrate
// whatever the link points at).
func listResources(dir, name string) (paths []string, warnings []string) {
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skill %q: walking %s: %v", name, p, err))
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil || rel == "SKILL.md" {
			return nil
		}
		if !d.Type().IsRegular() {
			warnings = append(warnings, fmt.Sprintf("skill %q: skipping non-regular file %s (symlinks are not staged)", name, rel))
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			warnings = append(warnings, fmt.Sprintf("skill %q: stat %s: %v", name, rel, ierr))
			return nil
		}
		if info.Size() > fileCap {
			warnings = append(warnings, fmt.Sprintf("skill %q: skipping %s (%d bytes > %d per-file cap)", name, rel, info.Size(), int64(fileCap)))
			return nil
		}
		if total+info.Size() > totalCap {
			warnings = append(warnings, fmt.Sprintf("skill %q: resource cap (%d bytes) reached — %s and later files are not staged", name, int64(totalCap), rel))
			return fs.SkipAll
		}
		total += info.Size()
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(paths)
	return paths, warnings
}

// Sources names the three discovery layers, in precedence order: an explicit
// -skills entry beats a project skill beats a user skill on a name collision
// (the more deliberate choice wins). An empty field skips that layer —
// callers set ProjectDir to "" on an -untrusted run, because a cloned repo
// must not inject standing instructions (SKILLS.md §D3).
type Sources struct {
	UserDir    string   // e.g. $XDG_CONFIG_HOME/driver-os/skills
	ProjectDir string   // e.g. <root>/.agents/skills; "" when untrusted
	Explicit   []string // -skills entries: parent dirs OR single skill folders
}

// Discover loads every valid skill across the source layers and returns the
// merged, name-sorted set plus human-readable warnings (invalid skills,
// collisions, oversize findings). It never fails: no sources, no skills.
func Discover(src Sources) (skills []*Skill, warnings []string) {
	byName := map[string]*Skill{}
	add := func(s *Skill, layer string) {
		if prev, ok := byName[s.Name]; ok {
			warnings = append(warnings, fmt.Sprintf("skill %q from %s overrides %s (%s layer wins)", s.Name, s.Dir, prev.Dir, layer))
		}
		byName[s.Name] = s
	}
	loadInto := func(dir, layer string) {
		s, warns, err := Load(dir)
		warnings = append(warnings, warns...)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping invalid skill at %s: %v", dir, err))
			return
		}
		add(s, layer)
	}
	scanParent := func(parent, layer string) {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return // an absent layer is normal, not a warning.
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(parent, e.Name())
			if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
				continue // not a skill folder; ignore quietly.
			}
			loadInto(dir, layer)
		}
	}

	if src.UserDir != "" {
		scanParent(src.UserDir, "user")
	}
	if src.ProjectDir != "" {
		scanParent(src.ProjectDir, "project")
	}
	for _, entry := range src.Explicit {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(entry, "SKILL.md")); err == nil {
			loadInto(entry, "explicit") // a single skill folder.
			continue
		}
		if _, err := os.Stat(entry); err != nil {
			warnings = append(warnings, fmt.Sprintf("-skills entry %q: %v", entry, err))
			continue
		}
		scanParent(entry, "explicit") // a parent dir of skill folders.
	}

	for _, s := range byName {
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, warnings
}

// UserDir is the operator-level skills layer shared by every binary that
// wires skills: $XDG_CONFIG_HOME/driver-os/skills (the same $XDG/driver-os
// convention the council corpus uses). Empty when the config dir can't be
// resolved — Discover treats that as an absent layer.
func UserDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "driver-os", "skills")
}

// ProjectDir is the repo-local skills layer for a given project root, at the
// vendor-neutral .agents/skills path (the one Codex and Gemini CLI also
// scan). Callers on an UNTRUSTED run must pass "" instead: a skill body is
// standing instructions injected into the agent's context, and a hostile
// checkout must not get that power (SKILLS.md §D3).
func ProjectDir(root string) string {
	return filepath.Join(root, ".agents", "skills")
}

// readCapped reads at most max bytes of a file, erroring if it is larger —
// a SKILL.md over the cap is malformed by definition, not worth truncating.
func readCapped(path string, max int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > max {
		return nil, fmt.Errorf("file is %d bytes, over the %d cap", info.Size(), max)
	}
	return os.ReadFile(path)
}

// clipRunes truncates s to at most n runes, appending "…" when it clipped.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
