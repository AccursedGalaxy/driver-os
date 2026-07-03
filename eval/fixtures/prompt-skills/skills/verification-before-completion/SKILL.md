---
name: verification-before-completion
description: Before declaring a bug fix or change complete, verify it actually satisfies the underlying requirement stated in the task or docstring — not just that the existing/given tests pass. Use whenever fixing a bug, implementing a stated invariant or contract, or finishing any task where tests-green could still hide a real defect because the given test suite is thin, missing edge cases, or does not directly exercise the stated contract.
---

# Verification Before Completion

Existing tests passing is necessary, but it is not enough. A test suite can be too weak to catch the real defect: it may cover only a few cases, skip boundaries, miss repeated or bulk operations, or have no concurrent coverage even when concurrency matters.

Before saying the work is complete, identify the exact contract the change is supposed to satisfy. Look for explicit claims in the task text, issue, or doc comment, such as:

- "must never exceed N"
- "must return X for every Y"
- "must not call twice after success"
- "must preserve order"
- "must be safe when called repeatedly"

Then ask:

> What specific claim does the task or docstring make that the given tests do NOT check?

If there is a gap, write and run one small throwaway check that directly exercises that claim. Keep it targeted: a short loop, one boundary case, one repeated-call probe, one bulk insert, or one minimal stress check. Do not turn this into a broad new test suite, coverage project, or refactor.

If the targeted check shows the invariant holds, say so plainly and finish. If it fails, the task is not done. Go back, fix the defect, and rerun both the existing tests and the targeted check. Do not report success just because the given tests pass.

## Worked example

Suppose you fixed a cache whose doc comment says:

> The cache must never hold more than Max entries.

The given tests only check that `Put` and `Get` return the right values for a few keys. They never check `Len()` after many distinct inserts, so tests-green does not prove the stated capacity invariant.

Before finishing, run one small check like:

```go
c := NewCache(3)
for i := 0; i < 100; i++ {
    c.Put(fmt.Sprintf("k%d", i), i)
    if c.Len() > 3 {
        t.Fatalf("cache length = %d, want <= 3", c.Len())
    }
}
```

If that check passes, you can say the capacity invariant was spot-checked and holds. If it fails, fix the cache; do not claim completion based only on the existing `Get`/`Put` tests.

Keep this discipline bounded: verify the one stated claim most likely to be missed by the supplied tests, then stop.
