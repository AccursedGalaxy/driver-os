package agent

import "fmt"

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

// stdoutObserver prints the exact lines the old single-file loop printed, so
// cmd/agent behaves identically after the extraction. oneLine lives in the
// agent package, so the observer stays a thin formatter over it.
type stdoutObserver struct{}

// NewStdoutObserver returns an Observer that prints the live loop trace to
// stdout — the CLI's rendering of a Run.
func NewStdoutObserver() Observer { return stdoutObserver{} }

func (stdoutObserver) Iteration(i, max int) {
	fmt.Printf("\n========== iteration %d/%d ==========\n", i, max)
}
func (stdoutObserver) Model(reply string)      { fmt.Printf("model: %s\n", reply) }
func (stdoutObserver) Observation(text string) { fmt.Printf("observation: %s\n", oneLine(text)) }
func (stdoutObserver) Note(msg string)         { fmt.Println(msg) }
func (stdoutObserver) Done(answer string)      { fmt.Printf("\n>>> DONE: %s\n", answer) }
