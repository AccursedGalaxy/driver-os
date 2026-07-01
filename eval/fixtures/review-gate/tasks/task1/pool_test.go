package pipeline

import "testing"

func double(x int) int { return x * 2 }

func checkAllDoubled(t *testing.T, items, got []int) {
	t.Helper()
	if len(got) != len(items) {
		t.Fatalf("got %d results for %d items", len(got), len(items))
	}
	for i := range items {
		if got[i] != items[i]*2 {
			t.Errorf("item %d: got %d, want %d", i, got[i], items[i]*2)
		}
	}
}

func TestProcessEvenSplit(t *testing.T) {
	items := []int{1, 2, 3, 4, 5, 6}
	checkAllDoubled(t, items, Process(items, 3, double))
}

func TestProcessUnevenSplit(t *testing.T) {
	items := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	checkAllDoubled(t, items, Process(items, 3, double))
}

func TestProcessMoreWorkersThanItems(t *testing.T) {
	items := []int{7, 9}
	checkAllDoubled(t, items, Process(items, 8, double))
}

func TestProcessSingleWorker(t *testing.T) {
	items := []int{5, 4, 3}
	checkAllDoubled(t, items, Process(items, 1, double))
}
