package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// gateContext unit tests: every property the contract requires (parent deadline
// expiry does NOT cancel; parent explicit cancel DOES; timeout fires; already-
// canceled parent propagates correctly; CancelFunc releases the AfterFunc).

func TestGateContextDeadlineExpiryDoesNotCancel(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	<-parent.Done() // deadline has expired

	// The returned context must NOT be canceled just because the parent deadline
	// expired — that is the whole reason for WithoutCancel in the implementation.
	gctx, gcancel := gateContext(parent, time.Second)
	defer gcancel()

	if gctx.Err() != nil {
		t.Fatalf("gateContext canceled on parent deadline expiry: %v", gctx.Err())
	}
}

func TestGateContextExplicitCancelPropagates(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())

	gctx, gcancel := gateContext(parent, time.Minute)
	defer gcancel()

	cancel() // explicit user cancel, not a deadline expiry

	// The AfterFunc fires in its own goroutine; wait for propagation.
	select {
	case <-gctx.Done():
	case <-time.After(time.Second):
		t.Fatal("gateContext not canceled within 1s of explicit parent cancel")
	}

	if err := gctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("gateContext not canceled after explicit parent cancel: %v", err)
	}
}

func TestGateContextTimeoutFires(t *testing.T) {
	parent := context.Background()

	gctx, gcancel := gateContext(parent, 10*time.Millisecond)
	defer gcancel()

	<-gctx.Done()

	if err := gctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gateContext timeout did not fire; err = %v", err)
	}
}

func TestGateContextAlreadyCanceledParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	gctx, gcancel := gateContext(parent, time.Minute)
	defer gcancel()

	// The parent is already canceled with context.Canceled, so the AfterFunc
	// fires immediately (in its own goroutine). Wait for propagation.
	select {
	case <-gctx.Done():
	case <-time.After(time.Second):
		t.Fatal("gateContext not canceled within 1s of already-canceled parent")
	}
}

func TestGateContextCancelFuncReleasesAfterFunc(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())

	gctx, gcancel := gateContext(parent, time.Minute)

	// Cancelling via the returned CancelFunc must release the AfterFunc: a later
	// parent cancel must NOT re-cancel the (already finished) derived context.
	gcancel()
	if gctx.Err() == nil {
		t.Fatal("gateContext should be canceled after CancelFunc")
	}

	// Now cancel the parent — this must not panic and must not affect the
	// already-finalized gctx.
	cancel()

	// Give any stray goroutine a moment to try to fire.
	time.Sleep(20 * time.Millisecond)

	// gctx should still be canceled (from the CancelFunc call).
	if gctx.Err() == nil {
		t.Fatal("gateContext should remain canceled after parent cancel")
	}
}

func TestGateContextDeadlineExpiryDoesNotCancelButExplicitCancelDoes(t *testing.T) {
	// Combined scenario: parent has a deadline AND explicit cancel.
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Minute)

	gctx, gcancel := gateContext(parent, time.Minute)
	defer gcancel()

	// Cancel explicitly — the derived context must cancel.
	cancel()

	select {
	case <-gctx.Done():
	case <-time.After(time.Second):
		t.Fatal("gateContext not canceled within 1s of explicit parent cancel")
	}

	if err := gctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("gateContext should be canceled after explicit parent cancel: %v", err)
	}
}

// hangingReviewer blocks on ctx.Done() and returns the context error — it
// simulates a Reviewer implementation that waits indefinitely for context
// cancellation (a hung network call, a stuck model, etc.).
type hangingReviewer struct{}

func (h *hangingReviewer) Review(ctx context.Context, _ ReviewInput) (*ReviewVerdict, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestGateContextAbortsHangingReviewer verifies at the unit level that a
// blocking function receiving a gateContext-derived context is aborted when
// the timeout fires, and that the error is context.DeadlineExceeded.
func TestGateContextAbortsHangingReviewer(t *testing.T) {
	const shortTimeout = 50 * time.Millisecond

	parent := context.Background()
	gctx, gcancel := gateContext(parent, shortTimeout)
	defer gcancel()

	hr := &hangingReviewer{}
	start := time.Now()
	_, err := hr.Review(gctx, ReviewInput{Task: "test", Diff: "diff", Root: "/tmp"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("hanging reviewer must return an error after timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded from hanging reviewer, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("hanging reviewer took %v; expected timeout in ~%v", elapsed, shortTimeout)
	}
}

// TestGateContextReviewerFailOpenIntegration verifies the full path: when
// Reviewer.Review returns an error, the run finishes with the existing
// fail-open verdict rather than blocking. Uses a real git workspace so
// captureDiff succeeds and the Reviewer.Review call is reached. The
// hanging+timeout case is covered at the unit level above; this test
// covers the error→failOpen integration.
func TestGateContextReviewerFailOpenIntegration(t *testing.T) {
	sb, root := gitWorkspace(t, map[string]string{"calc.go": "package calc\n"})
	t.Cleanup(func() { sb.Close() })

	// errReviewer returns an error on every Review call.
	errReviewer := &fakeReviewer{err: errors.New("review infrastructure fault")}

	g, err := newGateOld(context.Background(), Config{
		Obs:          nopObserver{},
		Task:         "fix it",
		Root:         root,
		Sandbox:      sb,
		Reviewer:     errReviewer,
		ReviewPolicy: ReviewPolicyFailOpen,
	})
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}

	// Mutate the workspace so the diff is non-empty, ensuring the reviewer
	// path is reached (not the "no changes" early return).
	if err := sb.WriteFile(context.Background(), "calc.go", []byte("package calc // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := g.Check(context.Background())

	// The gate must not block — fail open means the review error is recorded
	// but the verdict is not "blocked" on review alone.
	if rep.Blocked {
		t.Errorf("gate Blocked = true, want false (fail open on review error)")
	}
	if rep.Review == nil {
		t.Fatal("Review report is nil — should record the skip reason")
	}
	if rep.Review.Skipped == "" {
		t.Error("review skip reason should be recorded by failOpen")
	}
	t.Logf("fail-open skip: %s", rep.Review.Skipped)
	t.Logf("Blocked: %v, FenceViolation: %q, VerifyReason: %q", rep.Blocked, rep.FenceViolation, rep.VerifyReason)
}
