package agent

import (
	"fmt"
	"io"
	"os"

	"github.com/AccursedGalaxy/driver-os/llm"
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

// DeltaObserver is an OPTIONAL Observer extension (DESIGN decision 2: optional
// capabilities discovered via type-assertion). An Observer that ALSO implements it
// receives incremental text deltas as the model streams — the live-typing channel
// a chat REPL renders. The loop type-asserts for it and streams only when both
// Config.Stream is set and the provider supports streaming; an Observer that
// doesn't implement it is unaffected and still gets the whole reply via Model().
// Keeping it separate from Observer means none of the existing implementations
// (ndjson, issue-bot, commit-msg, jarvis, duet) need to change.
type DeltaObserver interface {
	ModelDelta(delta string) // one incremental text fragment from the streaming model call.
}

// VerifyObserver is an OPTIONAL Observer extension, discovered by
// type-assertion like DeltaObserver: an Observer that ALSO implements it
// receives the outcome of every VerifyCmd execution — the finish-gate checks
// (verifyTermination) and the kill/cap upgrade attempts (upgradeIfVerified)
// alike. It exists for the TUI's verify chip: a front-end that shows the gate's
// live green/red must not parse Note prose (a PASSING verification emits no
// Note at all). ok is the command's ground truth: it started, ran, and exited
// clean; a command that could not even start reports ok=false — the gate could
// not confirm success, which is exactly what the chip should say.
type VerifyObserver interface {
	VerifyResult(cmd string, ok bool)
}

// VerifyStartObserver is an OPTIONAL Observer extension, discovered by
// type-assertion like VerifyObserver: an Observer that ALSO implements it is
// told immediately BEFORE a VerifyCmd executes. It exists for live front-ends:
// a closing suite can run for minutes after the model's final token, and a
// spinner that keeps saying "running" makes gate time indistinguishable from
// model time. The matching VerifyResult (or the next iteration, for the
// pre-flight baseline) ends the phase.
type VerifyStartObserver interface {
	VerifyStart(cmd string)
}

// UsageObserver is an OPTIONAL Observer extension, discovered by type-assertion
// like DeltaObserver/VerifyObserver. It receives typed per-model-call usage for
// live UI telemetry; the string note path stays intact for observers that print
// the CLI trace.
type UsageObserver interface {
	// StepUsage reports cumulative turn usage after each model call, plus the
	// live context size (this call's prompt+completion). ctxTokens is separate
	// because cumulative.PromptTokens SUMS across iterations.
	StepUsage(cumulative llm.Usage, ctxTokens int)
}

// NoteKind labels the loop's machine-recognizable Note lines so a front-end
// can route them without parsing prose. Only the notes that duplicate a typed
// event carry a kind today; everything else is NoteGeneral.
type NoteKind string

const (
	NoteGeneral     NoteKind = ""             // an unclassified status line.
	NoteUsage       NoteKind = "usage"        // the per-turn "tokens: …" report (typed twin: UsageObserver.StepUsage).
	NoteReviewRound NoteKind = "review-round" // the "review: round n/m — …" summary (typed twin: ReviewObserver.ReviewVerdict).
)

// KindedNoteObserver is an OPTIONAL Observer extension, discovered by
// type-assertion like DeltaObserver. An Observer that implements it receives
// kinded notes through KindedNote INSTEAD of Note — the explicit opt-in that
// lets a front-end drop the notes whose content it already renders from typed
// events (usage, review round summaries) without string-prefix matching.
// Implementing UsageObserver/ReviewObserver alone changes nothing: observers
// that do not opt in keep receiving every line through Note, byte-identical.
type KindedNoteObserver interface {
	KindedNote(kind NoteKind, msg string)
}

// notifyNote routes one status line: observers that opted into kinds get the
// kind alongside the text; everyone else gets the plain Note line.
func notifyNote(obs Observer, kind NoteKind, msg string) {
	if k, ok := obs.(KindedNoteObserver); ok {
		k.KindedNote(kind, msg)
		return
	}
	obs.Note(msg)
}

// notifyVerify forwards one VerifyCmd outcome to the observer when it opted in.
func notifyVerify(obs Observer, cmd string, ok bool) {
	if v, ok2 := obs.(VerifyObserver); ok2 {
		v.VerifyResult(cmd, ok)
	}
}

// notifyVerifyStart forwards one about-to-run VerifyCmd to the observer when it
// opted in.
func notifyVerifyStart(obs Observer, cmd string) {
	if v, ok := obs.(VerifyStartObserver); ok {
		v.VerifyStart(cmd)
	}
}

// notifyUsage forwards one model-call usage update to the observer when it opted in.
func notifyUsage(obs Observer, cumulative llm.Usage, ctxTokens int) {
	if u, ok := obs.(UsageObserver); ok {
		u.StepUsage(cumulative, ctxTokens)
	}
}

// deltaSink returns the observer's ModelDelta method when it implements
// DeltaObserver, else nil. The streaming collector calls it per text chunk; nil
// means "collect silently" (a non-streaming observer, or a silent run).
func deltaSink(obs Observer) func(string) {
	if d, ok := obs.(DeltaObserver); ok {
		return d.ModelDelta
	}
	return nil
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
// routes it to stderr (docs/specs/CLI-SCRIPTABLE.md D1: stdout is the data channel) while a
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
