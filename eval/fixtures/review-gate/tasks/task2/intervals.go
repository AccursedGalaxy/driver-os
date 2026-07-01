// Package intervals implements a set of integer points stored as
// non-overlapping half-open intervals. See SPEC.md for the full contract.
package intervals

// Set is a set of integer points held as sorted, non-overlapping,
// non-touching half-open intervals [start, end).
//
// TODO: implement per SPEC.md. The zero value must be a ready-to-use empty
// set.
type Set struct {
	// TODO: choose a representation.
}

// Insert adds every point of [start, end) to the set. See SPEC.md.
func (s *Set) Insert(start, end int) {
	panic("not implemented")
}

// Remove deletes every point of [start, end) from the set. See SPEC.md.
func (s *Set) Remove(start, end int) {
	panic("not implemented")
}

// Covered reports whether every point of [start, end) is in the set.
// See SPEC.md.
func (s *Set) Covered(start, end int) bool {
	panic("not implemented")
}

// Intervals returns the stored intervals in ascending order. See SPEC.md.
func (s *Set) Intervals() [][2]int {
	panic("not implemented")
}
