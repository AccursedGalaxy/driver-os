package pipeline

import "sync"

// Process applies fn to every item using the given number of workers and
// returns the results in input order. It must process every item exactly
// once, for any workers >= 1 and any input length.
func Process(items []int, workers int, fn func(int) int) []int {
	if workers < 1 {
		workers = 1
	}
	out := make([]int, len(items))
	chunk := len(items) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * chunk
		end := start + chunk
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				out[i] = fn(items[i])
			}
		}(start, end)
	}
	wg.Wait()
	return out
}
