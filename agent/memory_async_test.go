package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AccursedGalaxy/driver-os/memory"
)

// Review #4: the post-answer store must neither be dropped by a Ctrl-C at the
// instant the model answers, nor block the RunResult (and the CLI's emission)
// on an LLM round-trip.

// ctxStateMem records whether the ctx handed to Add was already dead.
type ctxStateMem struct {
	fakeMem
	sawDeadCtx bool
}

func (m *ctxStateMem) Add(ctx context.Context, msgs []memory.Message, sc memory.Scope) ([]memory.Fact, error) {
	m.sawDeadCtx = ctx.Err() != nil
	return m.fakeMem.Add(ctx, msgs, sc)
}

// blockingMem holds Add open until released, so a test can observe the store
// mid-flight.
type blockingMem struct {
	fakeMem
	release chan struct{}
}

func (m *blockingMem) Add(ctx context.Context, msgs []memory.Message, sc memory.Scope) ([]memory.Fact, error) {
	<-m.release
	return m.fakeMem.Add(ctx, msgs, sc)
}

func TestRememberSurvivesCallerCancel(t *testing.T) {
	// The answer is verified and delivered by the time remember runs — a cancel
	// arriving right then must not throw the extracted fact away.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mem := &ctxStateMem{}
	remember(ctx, &recordObserver{}, mem, agentScope, "task", "answer")
	if mem.sawDeadCtx {
		t.Error("remember ran the store on the caller's canceled ctx — the fact would be dropped on Ctrl-C")
	}
}

func TestRememberAsyncSignalsCompletion(t *testing.T) {
	mem := &blockingMem{release: make(chan struct{})}
	done := rememberAsync(context.Background(), &recordObserver{}, mem, agentScope, "task", "answer")
	select {
	case <-done:
		t.Fatal("the store handle closed before the store finished")
	default: // still in flight — the caller was NOT blocked. Correct.
	}
	close(mem.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the store handle never closed after the store finished")
	}
	if rememberAsync(context.Background(), &recordObserver{}, nil, agentScope, "t", "a") != nil {
		t.Error("nil memory must return a nil handle (nothing to await)")
	}
	var noRun *RunResult
	noRun.AwaitMemory() // nil-safe: must not panic or block.
}

func TestRunGroundedAnswerStoreIsAwaitable(t *testing.T) {
	// Loop-level: a grounded answer starts the background store; AwaitMemory
	// synchronizes with it, after which the store's status note is visible.
	obs := &recordObserver{}
	sp := &scripted{replies: []string{"run echo hi", "answer done"}}
	res, err := runT(context.Background(), Config{
		Model:         sp,
		Sandbox:       sbWith(t, nil),
		Memory:        fakeMem{},
		Obs:           obs,
		Task:          "t",
		MaxIterations: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Answered {
		t.Fatalf("outcome = %q, want answered", res.Outcome)
	}
	res.AwaitMemory()
	var sawStore bool
	for _, n := range obs.notes {
		if strings.Contains(n, "stored") {
			sawStore = true
		}
	}
	if !sawStore {
		t.Errorf("after AwaitMemory the store must have completed and reported; notes=%v", obs.notes)
	}
}
