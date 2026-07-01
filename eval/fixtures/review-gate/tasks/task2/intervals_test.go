package intervals

import (
	"reflect"
	"testing"
)

func want(t *testing.T, s *Set, expect [][2]int) {
	t.Helper()
	got := s.Intervals()
	if len(got) == 0 && len(expect) == 0 {
		return
	}
	if !reflect.DeepEqual(got, expect) {
		t.Fatalf("intervals = %v, want %v", got, expect)
	}
}

func TestEmptySet(t *testing.T) {
	var s Set
	want(t, &s, nil)
	if s.Covered(1, 2) {
		t.Fatal("empty set covers nothing")
	}
	if !s.Covered(3, 3) {
		t.Fatal("empty interval is trivially covered, even on an empty set")
	}
}

func TestInsertBasic(t *testing.T) {
	var s Set
	s.Insert(1, 5)
	want(t, &s, [][2]int{{1, 5}})
}

func TestInsertEmptyIsNoop(t *testing.T) {
	var s Set
	s.Insert(5, 5)
	s.Insert(7, 3)
	want(t, &s, nil)
}

func TestInsertDisjointStaySeparate(t *testing.T) {
	var s Set
	s.Insert(3, 4)
	s.Insert(1, 2)
	want(t, &s, [][2]int{{1, 2}, {3, 4}})
}

func TestInsertTouchingMerges(t *testing.T) {
	var s Set
	s.Insert(1, 3)
	s.Insert(3, 5)
	want(t, &s, [][2]int{{1, 5}})
}

func TestInsertOverlappingMerges(t *testing.T) {
	var s Set
	s.Insert(1, 4)
	s.Insert(2, 6)
	want(t, &s, [][2]int{{1, 6}})
}

func TestInsertContainedIsNoop(t *testing.T) {
	var s Set
	s.Insert(1, 10)
	s.Insert(3, 5)
	want(t, &s, [][2]int{{1, 10}})
}

func TestInsertSwallowsMany(t *testing.T) {
	var s Set
	s.Insert(1, 2)
	s.Insert(3, 4)
	s.Insert(5, 6)
	s.Insert(2, 5)
	want(t, &s, [][2]int{{1, 6}})
}

func TestRemoveSplitsMiddle(t *testing.T) {
	var s Set
	s.Insert(1, 10)
	s.Remove(4, 6)
	want(t, &s, [][2]int{{1, 4}, {6, 10}})
}

func TestRemoveExact(t *testing.T) {
	var s Set
	s.Insert(1, 5)
	s.Remove(1, 5)
	want(t, &s, nil)
}

func TestRemoveTruncatesEdges(t *testing.T) {
	var s Set
	s.Insert(2, 8)
	s.Remove(0, 3)
	want(t, &s, [][2]int{{3, 8}})
	s.Remove(7, 10)
	want(t, &s, [][2]int{{3, 7}})
}

func TestRemoveAcrossMany(t *testing.T) {
	var s Set
	s.Insert(1, 3)
	s.Insert(5, 7)
	s.Insert(9, 11)
	s.Remove(2, 10)
	want(t, &s, [][2]int{{1, 2}, {10, 11}})
}

func TestRemoveEmptyIsNoop(t *testing.T) {
	var s Set
	s.Insert(1, 5)
	s.Remove(3, 3)
	s.Remove(4, 2)
	want(t, &s, [][2]int{{1, 5}})
}

func TestRemoveFromEmpty(t *testing.T) {
	var s Set
	s.Remove(1, 5)
	want(t, &s, nil)
}

func TestRemoveTouchingBoundaryIsNoop(t *testing.T) {
	var s Set
	s.Insert(1, 5)
	s.Remove(5, 8) // [5,8) shares only the boundary point 5, which is not in the set
	want(t, &s, [][2]int{{1, 5}})
	s.Remove(0, 1) // likewise [0,1) ends where the interval starts
	want(t, &s, [][2]int{{1, 5}})
}

func TestCoveredExactAndBeyond(t *testing.T) {
	var s Set
	s.Insert(1, 5)
	for _, c := range []struct {
		a, b int
		ok   bool
	}{
		{1, 5, true},
		{2, 4, true},
		{4, 5, true},
		{1, 6, false},
		{0, 5, false},
		{5, 6, false},
		{3, 3, true},
	} {
		if got := s.Covered(c.a, c.b); got != c.ok {
			t.Errorf("Covered(%d,%d) = %v, want %v", c.a, c.b, got, c.ok)
		}
	}
}

func TestCoveredGapInMiddle(t *testing.T) {
	var s Set
	s.Insert(1, 3)
	s.Insert(4, 6)
	if s.Covered(2, 5) {
		t.Fatal("Covered must see the gap [3,4)")
	}
	if !s.Covered(4, 6) {
		t.Fatal("second interval fully covered")
	}
}

func TestReinsertAfterRemove(t *testing.T) {
	var s Set
	s.Insert(1, 10)
	s.Remove(4, 6)
	s.Insert(4, 6)
	want(t, &s, [][2]int{{1, 10}})
}
