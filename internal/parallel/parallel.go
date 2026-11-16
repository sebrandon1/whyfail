// Package parallel runs a bounded fan-out over a slice.
package parallel

import "sync"

// Do calls fn(i) for every i in [0, n) and returns when all calls finish.
// At most limit calls run at once.
//
// The token is taken before the goroutine starts, so the number of goroutines
// is bounded too, not just the work they do. fn must not panic.
func Do(n, limit int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if limit < 1 {
		limit = 1
	}
	if limit > n {
		limit = n
	}

	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
