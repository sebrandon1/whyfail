// Package parallel runs a bounded fan-out over a slice.
package parallel

import (
	"fmt"
	"sync"
)

// Do calls fn(i) for every i in [0, n) and returns when all calls finish.
// At most limit calls run at once.
//
// The token is taken before the goroutine starts, so the number of goroutines
// is bounded too, not just the work they do.
//
// A panic in one lane is recovered and reported as an error naming that lane;
// the remaining lanes still run. When several lanes fail, the error from the
// lowest index wins so the message does not depend on arrival order.
func Do(n, limit int, fn func(i int) error) error {
	if n <= 0 {
		return nil
	}
	if limit < 1 {
		limit = 1
	}
	if limit > n {
		limit = n
	}

	errs := make([]error, n)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			// recover must run in this goroutine's own deferred call; Do
			// cannot see a worker's panic from around wg.Wait().
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("worker %d panicked: %v", i, r)
				}
			}()
			errs[i] = fn(i)
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
