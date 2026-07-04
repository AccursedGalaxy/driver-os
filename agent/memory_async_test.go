package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AccursedGalaxy/mneme"
	"github.com/AccursedGalaxy/mneme/provider/fake"
	"github.com/AccursedGalaxy/mneme/store/sqlite"
)

// Review #4: the post-answer store must neither be dropped by a Ctrl-C at the
// instant the model answers, nor block the RunResult (and the CLI's emission)
// on an LLM round-trip.

// ctxStateMem records whether the ctx handed to Add was already dead.
type ctxStateMem struct {
	fakeMem
	sawDeadCtx bool
}

func (m *ctxStateMem) Add(ctx context.Context, msgs []mneme.Message, sc mneme.Scope) ([]mneme.Fact, error) {
	m.sawDeadCtx = ctx.Err() != nil
	return m.fakeMem.Add(ctx, msgs, sc)
}

// blockingMem holds Add open until released, so a test can observe the store
// mid-flight.
type blockingMem struct {
	fakeMem
	release chan struct{}
}

func (m *blockingMem) Add(ctx context.Context, msgs []mneme.Message, sc mneme.Scope) ([]mneme.Fact, error) {
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
	res, err := Run(context.Background(), Config{
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

// TestConcurrentAddSearchWithRealStore proves that concurrent Add (a background
// rememberAsync goroutine from the previous turn) and Search (the next turn's
// recall) on the same mneme Memory backed by a real SQLite store is safe.
//
// Safety guarantee: store/sqlite.Open documents "The returned *Store is safe for
// concurrent use by multiple goroutines" (WAL mode + busy_timeout). The mneme
// Memory interface states it is "safe for concurrent use to the extent its
// underlying store, LLM and embedder are" — and both the LLM and embedder used
// here are stateless HTTP clients. The store itself has TestConcurrentReadsAndWrites
// in the mneme sqlite package exercising concurrent Search+Insert under -race.
//
// This test runs with -race, so any data race in the pipeline (Add's store.Search
// → LLM → store.Insert racing with Search's embed → store.Search) is caught
// here. It is the agent-level regression pin for backlog A4.
func TestConcurrentAddSearchWithRealStore(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	llm := &fake.LLM{Responses: []string{
		fake.JSON("Alice adopted a beagle named Max"),
		fake.JSON("Alice works at Shopify"),
	}}
	mem, err := mneme.New(
		mneme.WithStore(st),
		mneme.WithLLM(llm),
		mneme.WithEmbedder(&fake.Embedder{D: 128}),
		mneme.WithStrategy(mneme.Additive), // one LLM call per Add; simpler for the race test
	)
	if err != nil {
		t.Fatalf("mneme.New: %v", err)
	}

	scope := mneme.Scope{UserID: "test-user"}

	// Seed one fact so Search always has at least one hit — we exercise the
	// store.Search code path in BOTH goroutines with real I/O.
	if _, err := mem.Add(context.Background(), []mneme.Message{
		{Role: "user", Content: "I work at Shopify"},
	}, scope); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	var wg sync.WaitGroup
	errc := make(chan error, 2)

	// Simulate rememberAsync: fire Add in the background.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := mem.Add(context.Background(), []mneme.Message{
			{Role: "user", Content: "I adopted a beagle named Max"},
		}, scope); err != nil {
			errc <- fmt.Errorf("background Add: %w", err)
		}
	}()

	// Simulate next-turn recall: Search concurrently with the Add.
	wg.Add(1)
	go func() {
		defer wg.Done()
		hits, err := mem.Search(context.Background(), "where does Alice work", scope, 5)
		if err != nil {
			errc <- fmt.Errorf("concurrent Search: %w", err)
			return
		}
		if len(hits) == 0 {
			errc <- fmt.Errorf("Search returned no hits for seeded fact")
		}
	}()

	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
}
