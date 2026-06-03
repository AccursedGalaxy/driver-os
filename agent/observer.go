package agent

import (
	"fmt"
	"io"
	"os"
)

// Observer receives live progress events from a Run. It is the seam that lets
// ONE loop serve two callers: the CLI passes a printing Observer to reproduce
// the old think->act->observe output exactly, while the eval harness passes a
// silent one so a run yields data (the RunResult), not stdout noise. Run never
// calls fmt itself — every user-facing line goes through here.
type Observer interface {
	Iteration(i, max int)    // a new turn begins.
	Model(reply string)      // the model's raw reply this turn.
	Observation(text string) // a tool result fed back (Run hands the FULL text; the observer decides how to render).
	Note(msg string)         // a miscellaneous status line (e.g. the not-stored memory note).
	Done(answer string)      // the model emitted a final answer.
}

// nopObserver discards every event. Run substitutes it when Config.Obs is nil,
// so the loop can call the observer unconditionally.
type nopObserver struct{}

func (nopObserver) Iteration(int, int) {}
func (nopObserver) Model(string)       {}
func (nopObserver) Observation(string) {}
func (nopObserver) Note(string)        {}
func (nopObserver) Done(string)        {}

// writerObserver prints the exact lines the old single-file loop printed, so
// cmd/agent behaves identically after the extraction — but to a CALLER-CHOSEN
// writer. The live trace is diagnostics, not the run's data payload, so the CLI
// routes it to stderr (CLI-SCRIPTABLE.md D1: stdout is the data channel) while a
// human still sees it on the terminal. oneLine lives in the agent package, so the
// observer stays a thin formatter over it.
type writerObserver struct{ w io.Writer }

// NewWriterObserver returns an Observer that prints the live loop trace to w.
func NewWriterObserver(w io.Writer) Observer { return writerObserver{w} }

// NewStdoutObserver returns an Observer that prints the live loop trace to
// stdout. Retained for callers that want the historical stdout rendering;
// cmd/agent now routes the trace to stderr via NewWriterObserver (D1).
func NewStdoutObserver() Observer { return writerObserver{os.Stdout} }

func (o writerObserver) Iteration(i, max int) {
	fmt.Fprintf(o.w, "\n========== iteration %d/%d ==========\n", i, max)
}
func (o writerObserver) Model(reply string) { fmt.Fprintf(o.w, "model: %s\n", reply) }
func (o writerObserver) Observation(text string) {
	fmt.Fprintf(o.w, "observation: %s\n", oneLine(text))
}
func (o writerObserver) Note(msg string)    { fmt.Fprintln(o.w, msg) }
func (o writerObserver) Done(answer string) { fmt.Fprintf(o.w, "\n>>> DONE: %s\n", answer) }
