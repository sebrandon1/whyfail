package parallel

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRunsEveryIndexOnce(t *testing.T) {
	const n = 101
	var hits [n]int32
	Do(n, 7, func(i int) { atomic.AddInt32(&hits[i], 1) })
	for i, h := range hits {
		if h != 1 {
			t.Fatalf("index %d ran %d times", i, h)
		}
	}
}

func TestDoRunsConcurrently(t *testing.T) {
	const limit = 4
	arrived := make(chan struct{}, limit)
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		Do(limit, limit, func(int) {
			arrived <- struct{}{}
			<-release
		})
		close(done)
	}()

	// A serial implementation would block on the first call and never fill the
	// buffer, so this is a real assertion rather than a timing guess.
	for i := 0; i < limit; i++ {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d calls started at once", i, limit)
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Do did not return")
	}
}

func TestDoBoundsConcurrency(t *testing.T) {
	const n, limit = 200, 5
	var inFlight, peak int32

	Do(n, limit, func(int) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		atomic.AddInt32(&inFlight, -1)
	})

	if peak > limit {
		t.Fatalf("peak concurrency %d exceeds the limit %d", peak, limit)
	}
}

func TestDoNoItems(t *testing.T) {
	called := false
	Do(0, 4, func(int) { called = true })
	if called {
		t.Fatal("fn was called with no items")
	}
}

func TestDoClampsLimit(t *testing.T) {
	var ran int32
	Do(5, 0, func(int) { atomic.AddInt32(&ran, 1) })
	if ran != 5 {
		t.Fatalf("ran %d times, want 5", ran)
	}
}
