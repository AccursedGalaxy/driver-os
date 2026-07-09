package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

const (
	// envMaxEntries caps the top-level listing in the ENVIRONMENT preamble so a
	// huge workspace root can't bloat the opening message (P1).
	envMaxEntries = 40
	// envObserveTimeout bounds the whole pre-turn observation: a slow backend
	// must not stall the run before the first model call.
	envObserveTimeout = 5 * time.Second
)

// observeEnvironment gathers the opening ENVIRONMENT preamble through the
// sandbox, BEFORE the first model turn: the working directory `run` commands
// start in (the WorkdirReporter capability) and the root's top-level entries
// (ListDir — no subprocess). Seeding these into the opening message kills two
// failure modes observed live: the near-universal "turn 1 = list_dir ." tax
// (paid by 3/3 runs in the 2026-07-01 hard-task probe), and the model guessing
// an absolute cwd (`cd /home/user && go test ./...` — two wasted turns).
//
// Everything is BEST-EFFORT: a part that can't be observed is omitted, and a
// nil sandbox — or one exposing neither piece — yields "" so the seed message
// is exactly the historical "TASK: …". This preamble is harness-observed, not
// model-observed: it does NOT count toward the grounded gate (P4), which stays
// about what the MODEL verified with tools this run.
func observeEnvironment(ctx context.Context, sb sandbox.Sandbox, includeDeps bool) string {
	if sb == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, envObserveTimeout)
	defer cancel()

	var b strings.Builder
	if wr, ok := sb.(sandbox.WorkdirReporter); ok {
		if wd := wr.Workdir(); wd != "" {
			fmt.Fprintf(&b, "\n- working directory: %s — `run` commands START here, and file-tool paths are relative to it; do NOT cd elsewhere to find the project", wd)
		}
	}
	if entries, err := sb.ListDir(ctx, "."); err == nil && len(entries) > 0 {
		hasGoMod := false
		for _, e := range entries {
			if e.Name == "go.mod" && !e.IsDir {
				hasGoMod = true
				break
			}
		}

		shown, more := entries, 0
		if len(entries) > envMaxEntries {
			shown, more = entries[:envMaxEntries], len(entries)-envMaxEntries
		}
		names := make([]string, 0, len(shown))
		for _, e := range shown {
			kind := "file"
			if e.IsDir {
				kind = "dir"
			}
			names = append(names, kind+" "+e.Name)
		}
		line := strings.Join(names, ", ")
		if more > 0 {
			line += fmt.Sprintf(", … (+%d more — list_dir \".\" for the rest)", more)
		}
		fmt.Fprintf(&b, "\n- top-level entries: %s", line)

		if includeDeps && hasGoMod {
			if digest := observeDependencies(ctx, sb); digest != "" {
				b.WriteString(digest)
			}
		}
	}

	if b.Len() == 0 {
		return ""
	}
	return "\n\nENVIRONMENT (observed at start):" + b.String()
}

// observeDependencies attempts to gather a dependency digest for the ENVIRONMENT
// preamble. It is best-effort: any failure results in an empty string.
func observeDependencies(ctx context.Context, sb sandbox.Sandbox) string {
	workdir := hostWorkspace(sb)
	// go list -m -json all emits a stream of JSON objects.
	// We set GOSUMDB=off for extra offline safety.
	out, err := runGoToolEnv(ctx, workdir, []string{"GOSUMDB=off"}, []string{"list", "-m", "-json", "all"})
	if err != nil {
		return ""
	}
	return formatDepDigest(out, 30)
}

type goModule struct {
	Path      string
	Version   string
	Main      bool
	Indirect  bool
	GoVersion string
	Replace   *struct {
		Path    string
		Version string
	}
}

func formatDepDigest(goListJSON string, cap int) string {
	dec := json.NewDecoder(strings.NewReader(goListJSON))
	var main *goModule
	var direct []goModule

	for {
		var m goModule
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return "" // Parse error
		}
		if m.Main {
			if main == nil {
				mCopy := m
				main = &mCopy
			}
		} else if !m.Indirect {
			direct = append(direct, m)
		}
	}

	if main == nil {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n- dependencies (module %s, go %s; effective versions):", main.Path, main.GoVersion)

	shown := direct
	more := 0
	if len(direct) > cap {
		shown = direct[:cap]
		more = len(direct) - cap
	}

	for _, d := range shown {
		if d.Replace != nil {
			target := d.Replace.Path
			if d.Replace.Version != "" {
				target += "@" + d.Replace.Version
			}
			fmt.Fprintf(&b, "\n    %s => %s", d.Path, target)
		} else {
			fmt.Fprintf(&b, "\n    %s %s", d.Path, d.Version)
		}
	}

	if more > 0 {
		fmt.Fprintf(&b, "\n    … (+%d more direct; go_doc a package for details)", more)
	}

	b.WriteString("\n- library versions above are pinned; before relying on an external Go library API, confirm it with the go_doc tool at that version — training data may be stale.")
	return b.String()
}

func verifySkipFilter(verifyCmd string) string {
	for i := 0; i < len(verifyCmd); {
		start := strings.Index(verifyCmd[i:], "-skip")
		if start < 0 {
			return ""
		}
		start += i
		if start > 0 && verifyCmd[start-1] != ' ' && verifyCmd[start-1] != '\t' {
			i = start + len("-skip")
			continue
		}

		arg := start + len("-skip")
		if arg == len(verifyCmd) {
			return ""
		}
		if verifyCmd[arg] == '=' {
			arg++
		} else if verifyCmd[arg] == ' ' || verifyCmd[arg] == '\t' {
			for arg < len(verifyCmd) && (verifyCmd[arg] == ' ' || verifyCmd[arg] == '\t') {
				arg++
			}
		} else {
			i = arg
			continue
		}
		if arg == len(verifyCmd) {
			return ""
		}

		end := arg
		if verifyCmd[arg] == '\'' || verifyCmd[arg] == '"' {
			quote := verifyCmd[arg]
			end++
			for end < len(verifyCmd) && verifyCmd[end] != quote {
				end++
			}
			if end == len(verifyCmd) {
				return ""
			}
			end++
		} else {
			for end < len(verifyCmd) && verifyCmd[end] != ' ' && verifyCmd[end] != '\t' {
				end++
			}
		}
		if end > arg {
			return "-skip " + verifyCmd[arg:end]
		}
		return ""
	}
	return ""
}

func verifyGatePreamble(cfg Config) string {
	if cfg.VerifyCmd == "" {
		return ""
	}
	prov := ""
	if cfg.autoVerifyProvenance != "" {
		prov = " (auto-derived from " + cfg.autoVerifyProvenance + ")"
	}
	example := "go test ./pkg/you/changed/"
	hint := ""
	if filter := verifySkipFilter(cfg.VerifyCmd); filter != "" {
		example += " " + filter
		hint = " The gate deliberately skips these — carry the filter or you will see unrelated red tests."
	}
	return fmt.Sprintf("\n\nVERIFY GATE: `%s`%s. The harness runs this authoritatively when you finish. If you want mid-run signal, run ONLY tests scoped to the package(s) you changed (for example, `%s`).%s NEVER run the full suite or the full verify command yourself.", cfg.VerifyCmd, prov, example, hint)
}
