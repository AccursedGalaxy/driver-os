# intervals.Set — specification

`Set` maintains a set of integer points, represented as non-overlapping,
non-touching, half-open intervals `[start, end)` in ascending order.

Invariant: after any operation, for consecutive stored intervals
`[a,b)` and `[c,d)` it holds that `b < c` (a gap of at least one point).
Touching intervals (`b == c`) must have been merged into one.

## Operations

### `Insert(start, end int)`

Adds every point of `[start, end)` to the set.

- If `start >= end` the interval is empty: Insert is a no-op.
- Intervals that overlap **or touch** the inserted one are merged with it.
  E.g. inserting `[3,5)` into a set containing `[1,3)` yields `[1,5)`.
- One insert may swallow many stored intervals: inserting `[2,5)` into
  `{[1,2), [3,4), [5,6)}` yields `{[1,6)}`.

### `Remove(start, end int)`

Deletes every point of `[start, end)` from the set.

- If `start >= end`: no-op.
- Removing from the middle of a stored interval splits it in two:
  removing `[4,6)` from `{[1,10)}` yields `{[1,4), [6,10)}`.
- Removal may truncate several intervals and delete others entirely.
- Removing points not in the set is fine (no error).

### `Covered(start, end int) bool`

Reports whether **every** point of `[start, end)` is in the set.

- The empty interval (`start >= end`) is trivially covered: returns `true`,
  even on an empty set.
- `Covered` must be exact at boundaries: with `{[1,5)}`, `Covered(1,5)` is
  true but `Covered(1,6)` and `Covered(0,5)` are false.
- A gap anywhere in `[start, end)` makes it false, even if both ends are
  covered.

### `Intervals() [][2]int`

Returns the stored intervals as `[start, end)` pairs in ascending order.
Returns an empty (or nil) slice for an empty set. The caller may not rely on
mutating the returned slice.
