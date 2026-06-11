package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AccursedGalaxy/driver-os/agent"
	"github.com/AccursedGalaxy/driver-os/sandbox"
)

const (
	// StageDir is where a skill's bundled files land inside the sandbox, as a
	// project-root-relative path: StageDir/<name>/<resource>. One predictable
	// place, stated in the load header, so the model never hunts for scripts.
	StageDir = ".skills"

	// entryDescCap bounds ONE skill's description inside the listing (Claude
	// Code's number) — a single long-winded description must not starve the
	// rest of the listing.
	entryDescCap = 1536

	// fallbackListingCap bounds the WHOLE listing when the model's context
	// window is unknown (ctxWindow <= 0): Codex's fixed-char fallback. With a
	// known window the budget is ~1% of it (in chars, ~4 chars/token).
	fallbackListingCap = 8000

	// resourceListCap bounds how many staged paths the load header enumerates;
	// past it the model is told the count and where to list the rest.
	resourceListCap = 20
)

// Tool builds the single `skill` meta-tool over the discovered set. Its
// description carries the Level-1 listing (every skill's name + description);
// invoking it returns the skill's full SKILL.md body (Level 2) and lazily
// stages its bundled files into the sandbox at .skills/<name>/ so the agent's
// ordinary read_file/run tools cover Level 3. The handler runs HOST-side (the
// go_doc precedent): skill folders are operator configuration, not workspace
// content — the sandbox only ever receives a staged COPY.
//
// ctxWindow is the model's context window in tokens (0 = unknown), used only
// to budget the listing. Callers with an empty skill set should not install
// the tool at all, keeping skill-free runs byte-identical to today.
func Tool(skills []*Skill, sb sandbox.Sandbox, ctxWindow int) agent.Tool {
	byName := make(map[string]*Skill, len(skills))
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	sort.Strings(names)

	listing := buildListing(skills, ctxWindow)

	st := &state{loaded: map[string]bool{}}
	load := func(ctx context.Context, rawName string) (string, error) {
		return st.load(ctx, sb, byName, names, rawName)
	}

	enum, _ := json.Marshal(names)
	schema := fmt.Sprintf(`{"type":"object","properties":{`+
		`"name":{"type":"string","enum":%s,"description":"the skill to load, exactly as named in the skill listing"}},`+
		`"required":["name"]}`, enum)

	return agent.Tool{
		Name:       "skill",
		Desc:       "load a SKILL: expert, task-specific instructions written for exactly one kind of job. When the TASK matches a skill's description below, load it FIRST and follow it — it encodes procedure you do not otherwise know. Loading is cheap; guessing a procedure a skill covers is how runs fail. ARG: the skill name, nothing else. RETURNS: the skill's full instructions; any bundled files are staged under " + StageDir + "/<name>/ for read_file/run. Available skills: " + listing.text,
		NativeDesc: "Load a SKILL: expert, task-specific instructions written for exactly one kind of job. When the TASK matches a skill's description below, load it FIRST and follow it — it encodes procedure you do not otherwise know. Loading is cheap; guessing a procedure a skill covers is how runs fail. Returns the skill's full instructions; any bundled files are staged under " + StageDir + "/<name>/ for read_file/run.\n\nAvailable skills:\n" + listing.native,
		Run: func(ctx context.Context, arg string) (string, error) {
			return load(ctx, arg)
		},
		Schema: json.RawMessage(schema),
		RunJSON: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return "", fmt.Errorf("invalid skill arguments: %v", err)
			}
			return load(ctx, a.Name)
		},
	}
}

// state tracks which skills this RUN has already loaded (and therefore
// staged). Guarded: the native loop can dispatch parallel tool calls.
type state struct {
	mu     sync.Mutex
	loaded map[string]bool
}

func (st *state) load(ctx context.Context, sb sandbox.Sandbox, byName map[string]*Skill, names []string, rawName string) (string, error) {
	name := normalizeName(rawName)
	s, ok := byName[name]
	if !ok {
		// (P3) a teaching error: name the valid set, don't invite a blind retry.
		return "", fmt.Errorf("unknown skill %q — the available skills are: %s", rawName, strings.Join(names, ", "))
	}

	st.mu.Lock()
	already := st.loaded[name]
	st.mu.Unlock()

	stageRel := StageDir + "/" + name
	if already {
		// Idempotent and cheap: the body already sits in context above; do not
		// burn the window re-sending it (Claude Code's no-re-read behavior).
		return fmt.Sprintf("skill %q is already loaded above — its instructions still apply. Bundled files remain at %s/.", name, stageRel), nil
	}

	var resNote string
	if len(s.Resources) > 0 {
		if err := stage(ctx, sb, s, stageRel); err != nil {
			// (P6) a staging failure is an observation; the skill stays
			// unloaded so a retry next turn re-attempts the copy.
			return "", fmt.Errorf("staging skill %q resources: %v", name, err)
		}
		resNote = "\nBundled files staged under " + stageRel + "/ (read references with read_file, execute scripts with run; paths are relative to the project root):\n" + resourceList(s.Resources, stageRel)
	}

	st.mu.Lock()
	st.loaded[name] = true
	st.mu.Unlock()

	body := strings.NewReplacer(
		"${SKILL_DIR}", stageRel,
		"${CLAUDE_SKILL_DIR}", stageRel, // the Claude Code spelling, for unmodified imported skills.
	).Replace(s.Body)

	header := fmt.Sprintf("SKILL %q loaded — the instructions below are the procedure you asked for. Follow them where they apply to the TASK; verify any file paths they mention with tools before relying on them.%s", name, resNote)
	return header + "\n\n---\n" + body, nil
}

// stage copies a skill's bundled files into the sandbox under stageRel,
// preserving layout and execute bits. Directories are created with one mkdir
// -p exec (the Sandbox interface has no Mkdir); file bytes flow through
// WriteFile so every backend (local fence, docker) receives them identically.
func stage(ctx context.Context, sb sandbox.Sandbox, s *Skill, stageRel string) error {
	dirs := map[string]bool{stageRel: true}
	for _, rel := range s.Resources {
		d := stageRel + "/" + slashDir(rel)
		dirs[strings.TrimSuffix(d, "/")] = true
	}
	mkdir := make([]string, 0, len(dirs)+1)
	mkdir = append(mkdir, "-p")
	for d := range dirs {
		mkdir = append(mkdir, d)
	}
	sort.Strings(mkdir[1:])
	if res, err := sb.Exec(ctx, sandbox.Command{Path: "mkdir", Args: mkdir}); err != nil {
		return fmt.Errorf("mkdir: %v", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("mkdir exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	for _, rel := range s.Resources {
		src := filepath.Join(s.Dir, filepath.FromSlash(rel))
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("reading %s: %v", rel, err)
		}
		mode := os.FileMode(0o644)
		if info, err := os.Stat(src); err == nil && info.Mode()&0o111 != 0 {
			mode = 0o755 // keep scripts executable.
		}
		if err := sb.WriteFile(ctx, stageRel+"/"+rel, data, mode); err != nil {
			return fmt.Errorf("writing %s: %v", rel, err)
		}
	}

	excludeStaging(ctx, sb)
	return nil
}

// excludeStaging best-effort appends the staging dir to .git/info/exclude so
// staged skill files never pollute the project's git status or a -review
// diff. info/exclude is repo-LOCAL (never a tracked file — we must not edit
// the project's .gitignore). Every failure is silently fine: not a git repo,
// no .git/info dir, read-only fs — staging still worked.
func excludeStaging(ctx context.Context, sb sandbox.Sandbox) {
	const path = ".git/info/exclude"
	line := StageDir + "/"
	existing, err := sb.ReadFile(ctx, path)
	if err == nil && strings.Contains(string(existing), line) {
		return
	}
	if err != nil {
		if _, derr := sb.ListDir(ctx, ".git/info"); derr != nil {
			return // no .git/info — not a repo (or bare); nothing to exclude.
		}
		existing = nil
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	_ = sb.WriteFile(ctx, path, []byte(content+line+"\n"), 0o644)
}

// resourceList renders the staged paths for the load header, capped so a
// resource-heavy skill can't flood the observation.
func resourceList(resources []string, stageRel string) string {
	var b strings.Builder
	for i, r := range resources {
		if i == resourceListCap {
			fmt.Fprintf(&b, "  …%d more — `list_dir %s` to see everything\n", len(resources)-resourceListCap, stageRel)
			break
		}
		fmt.Fprintf(&b, "  %s/%s\n", stageRel, r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// normalizeName makes the text-loop arg forgiving without being fuzzy: trim
// whitespace and stray quoting, lowercase. "Humanizer" loads humanizer; an
// actually-wrong name still errors with the valid set.
func normalizeName(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "`'\"")
	return strings.ToLower(strings.TrimSpace(s))
}

// listing carries the two renderings of the Level-1 skill list: `text` is a
// single line (buildSystemPrompt prints one line per tool), `native` is
// line-per-skill (it lives inside the tool schema's description, where
// newlines are fine).
type listingOut struct {
	text   string
	native string
}

// buildListing renders name+description for every skill under the char
// budget: ~1% of the context window when known, else fallbackListingCap. Per
// entry the description is capped at entryDescCap. On overflow, descriptions
// are dropped from the alphabetically-LAST skill backward (no invocation
// stats yet to rank by); names are never dropped — the model must always be
// able to see that a skill exists.
func buildListing(skills []*Skill, ctxWindow int) listingOut {
	budget := fallbackListingCap
	if ctxWindow > 0 {
		budget = ctxWindow / 100 * 4 // ~1% of the window, ~4 chars/token.
	}

	sorted := append([]*Skill(nil), skills...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	descs := make([]string, len(sorted))
	total := 0
	for i, s := range sorted {
		d := s.Description
		if utf8.RuneCountInString(d) > entryDescCap {
			d = clipRunes(d, entryDescCap)
		}
		// The text rendering collapses a multi-line description (the spec
		// allows block scalars) onto one line.
		d = strings.Join(strings.Fields(d), " ")
		descs[i] = d
		total += len(sorted[i].Name) + len(d) + 4
	}
	// Overflow: drop whole descriptions (alphabetically-last first) until the
	// listing fits; a name-only entry still tells the model the skill EXISTS.
	for i := len(sorted) - 1; i >= 0 && total > budget; i-- {
		total -= len(descs[i])
		descs[i] = ""
	}

	var text, native strings.Builder
	for i, s := range sorted {
		if i > 0 {
			text.WriteString(" | ")
		}
		if descs[i] == "" {
			text.WriteString(s.Name)
			fmt.Fprintf(&native, "- %s\n", s.Name)
			continue
		}
		fmt.Fprintf(&text, "%s: %s", s.Name, descs[i])
		fmt.Fprintf(&native, "- %s: %s\n", s.Name, descs[i])
	}
	return listingOut{text: text.String(), native: strings.TrimRight(native.String(), "\n")}
}

// slashDir returns the slash-form directory of a slash-relative path, ""
// for a bare filename.
func slashDir(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}
