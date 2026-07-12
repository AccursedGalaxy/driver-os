package headless

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/AccursedGalaxy/driver-os/sandbox/gated"
	"github.com/AccursedGalaxy/driver-os/vcs"
)

// ttyIO is the controlling terminal, opened explicitly so interactive review
// prompts read/write /dev/tty rather than stdin/stdout (D1/D5). Two payoffs: stdout
// stays the machine data channel even while the human is prompted, and a task piped
// on stdin (`-task -`) never collides with an approval prompt — git, ssh and sudo
// do the same. One buffered reader is shared by the per-run approver and the final
// prompt so they don't each buffer ahead and steal each other's input.
type ttyIO struct {
	f *os.File
	r *bufio.Reader
}

// openTTY opens the controlling terminal for reading and writing. It fails when
// there is none (CI, cron, a bare pipe); the caller turns that into a fail-fast so
// an interactive policy never deadlocks on an absent terminal.
func openTTY() (*ttyIO, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &ttyIO{f: f, r: bufio.NewReader(f)}, nil
}

// prompt writes msg to the terminal and reads one line of the reply.
func (t *ttyIO) prompt(msg string) (string, error) {
	fmt.Fprint(t.f, msg)
	return t.r.ReadString('\n')
}

func (t *ttyIO) Close() error { return t.f.Close() }

// ttyApprover asks the human, on the controlling terminal, before a gated `run`
// command executes. It fails CLOSED: if the tty read fails (EOF), the command is
// denied, never silently allowed.
type ttyApprover struct{ tty *ttyIO }

func (a ttyApprover) Approve(_ context.Context, r gated.Request) (bool, error) {
	line, err := a.tty.prompt(fmt.Sprintf("\n⟫ the agent wants to run: %s\n  allow? [y/N] ", r.Display))
	if err != nil && strings.TrimSpace(line) == "" {
		return false, fmt.Errorf("no input on /dev/tty")
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "y" || s == "yes", nil
}

// reviewGate maps -approve to the (approver, policy) pair for the run gate (D5):
//   - interactive: DefaultPolicy auto-allows the safe allowlist; the rest is asked
//     on the tty.
//   - policy: DefaultPolicy with NO approver — the allowlist runs, everything else
//     fails closed (an Ask with no approver is blocked). Fully unattended.
//   - never: AskAll with no approver — every `run` is blocked.
func reviewGate(approve string, tty *ttyIO) (gated.Approver, gated.Policy) {
	switch approve {
	case "policy":
		return nil, gated.DefaultPolicy
	case "never":
		return nil, gated.AskAll
	default: // interactive
		return ttyApprover{tty}, gated.DefaultPolicy
	}
}

// reviewChanges stages everything the agent did, shows it as one diff (on stderr —
// it is diagnostic, not the run's data payload, D1), and resolves what to do with
// it per `action`: "prompt" asks on the tty; "commit"/"discard"/"keep" act without
// a human. It runs regardless of the agent's outcome, so even a failed run's partial
// edits can be discarded or inspected rather than silently left in the tree.
func reviewChanges(ctx context.Context, dir, commitMsg, action string, tty *ttyIO) error {
	if err := vcs.StageAll(ctx, dir); err != nil {
		return err
	}
	diff, err := vcs.Diff(ctx, dir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(diff) == "" {
		fmt.Fprintln(errW, "\nreview: the agent made no file changes — nothing to commit.")
		return vcs.Unstage(ctx, dir)
	}
	fmt.Fprintln(errW, "\n===== proposed changes (staged diff) =====")
	fmt.Fprintln(errW, diff)

	decision := action
	if action == "prompt" {
		line, _ := tty.prompt("\nreview: [c]ommit  [d]iscard  [k]eep unstaged (default) ? ")
		decision = promptDecision(line)
	}
	switch decision {
	case "commit":
		if err := vcs.Commit(ctx, dir, commitMsg); err != nil {
			return err
		}
		fmt.Fprintf(errW, "review: committed (%q).\n", commitMsg)
	case "discard":
		if err := vcs.Discard(ctx, dir); err != nil {
			return err
		}
		fmt.Fprintln(errW, "review: discarded — working tree restored to HEAD.")
	default: // keep
		if err := vcs.Unstage(ctx, dir); err != nil {
			return err
		}
		fmt.Fprintln(errW, "review: kept; changes left unstaged in the working tree for you to inspect.")
	}
	return nil
}

// promptDecision maps a free-text terminal reply to a review action, defaulting to
// keep — the safe, non-destructive choice — on anything unrecognized or empty.
func promptDecision(line string) string {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "c", "commit":
		return "commit"
	case "d", "discard":
		return "discard"
	default:
		return "keep"
	}
}

// commitMessage turns a task into a one-line commit subject: "agent: <first line of
// the task>", clipped so a paragraph-long task doesn't become a giant subject.
func commitMessage(task string) string {
	first := task
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	const max = 68 // 72 minus the "agent: " prefix, the git subject convention.
	if len(first) > max {
		first = strings.TrimSpace(first[:max]) + "…"
	}
	if first == "" {
		return "agent: changes"
	}
	return "agent: " + first
}
