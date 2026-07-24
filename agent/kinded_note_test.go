package agent

import "testing"

// kindedRecorder opts into kinded notes; plainRecorder does not. Together they
// pin the KindedNoteObserver contract: opting in REPLACES Note delivery for
// every routed line, and not opting in keeps the plain Note stream intact.
type kindedRecorder struct {
	Observer
	kinds []NoteKind
	msgs  []string
}

func (k *kindedRecorder) KindedNote(kind NoteKind, msg string) {
	k.kinds = append(k.kinds, kind)
	k.msgs = append(k.msgs, msg)
}

type plainRecorder struct {
	Observer
	notes []string
}

func (p *plainRecorder) Note(msg string) { p.notes = append(p.notes, msg) }

func TestNotifyNoteRoutesKindsToOptedInObserver(t *testing.T) {
	k := &kindedRecorder{}
	notifyNote(k, NoteUsage, "tokens: 12 cumulative after turn 1 (prompt 10, completion 2)")
	notifyNote(k, NoteReviewRound, "review: round 1/3 — 0 finding(s), 0 blocking, 0 advisory")
	notifyNote(k, NoteGeneral, "upgrade blocked — reason")

	if len(k.kinds) != 3 || k.kinds[0] != NoteUsage || k.kinds[1] != NoteReviewRound || k.kinds[2] != NoteGeneral {
		t.Fatalf("kinds = %v", k.kinds)
	}
}

func TestNotifyNoteFallsBackToPlainNote(t *testing.T) {
	p := &plainRecorder{}
	notifyNote(p, NoteUsage, "tokens: 12")
	notifyNote(p, NoteGeneral, "misc")

	if len(p.notes) != 2 || p.notes[0] != "tokens: 12" || p.notes[1] != "misc" {
		t.Fatalf("notes = %v", p.notes)
	}
}
