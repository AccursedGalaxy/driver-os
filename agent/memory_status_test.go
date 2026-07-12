package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/memory"
)

// fakeMem is a minimal memory.Store: Search returns one canned fact and Add
// reports one written fact — just enough to exercise recall/remember's status
// reporting without a real embeddings/LLM endpoint.
type fakeMem struct{}

func (fakeMem) Add(context.Context, []memory.Message, memory.Scope) ([]memory.Fact, error) {
	return []memory.Fact{{Text: "stored thing"}}, nil
}
func (fakeMem) Search(context.Context, string, memory.Scope, int) ([]memory.Fact, error) {
	return []memory.Fact{{Text: "recalled thing"}}, nil
}
func (fakeMem) Get(context.Context, string) (memory.Fact, error) { return memory.Fact{}, nil }
func (fakeMem) Delete(context.Context, string) error             { return nil }
func (fakeMem) Close() error                                     { return nil }

// recordObserver captures Note() lines so a test can assert memory status flows
// through the Observer, never stdout (the machine-readable data channel under
// -format=json/ndjson). See the harness review, finding #2.
type recordObserver struct{ notes []string }

func (r *recordObserver) Iteration(int, int) {}
func (r *recordObserver) Model(string)       {}
func (r *recordObserver) Observation(string) {}
func (r *recordObserver) Note(m string)      { r.notes = append(r.notes, m) }
func (r *recordObserver) Done(string)        {}

func TestMemoryStatusGoesThroughObserver(t *testing.T) {
	// recall/remember must report their status via Observer.Note, not fmt.Printf to
	// stdout — the package contract ("Run never calls fmt itself", observer.go) and
	// the reason a memory-enabled run under -format=json used to corrupt its output.
	obs := &recordObserver{}
	if got := recall(context.Background(), obs, fakeMem{}, agentScope, "task"); got == "" {
		t.Fatal("recall returned no memory block for a hit")
	}
	remember(context.Background(), obs, fakeMem{}, agentScope, "task", "answer")

	var sawRecall, sawStore bool
	for _, n := range obs.notes {
		if strings.Contains(n, "recalled") {
			sawRecall = true
		}
		if strings.Contains(n, "stored") {
			sawStore = true
		}
	}
	if !sawRecall {
		t.Errorf("recall status did not go through Observer.Note; notes=%v", obs.notes)
	}
	if !sawStore {
		t.Errorf("remember status did not go through Observer.Note; notes=%v", obs.notes)
	}
}
