package parallel

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRunsEveryIndexOnce(t *testing.T) {
	const n = 101
	var hits [n]int32
	if err := Do(n, 7, func(i int) error { atomic.AddInt32(&hits[i], 1); return nil }); err != nil {
		t.Fatalf("Do: %v", err)
	}
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
		if err := Do(limit, limit, func(int) error {
			arrived <- struct{}{}
			<-release
			return nil
		}); err != nil {
			t.Errorf("Do: %v", err)
		}
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

	err := Do(n, limit, func(int) error {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		atomic.AddInt32(&inFlight, -1)
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if peak > limit {
		t.Fatalf("peak concurrency %d exceeds the limit %d", peak, limit)
	}
}

func TestDoNoItems(t *testing.T) {
	called := false
	if err := Do(0, 4, func(int) error { called = true; return nil }); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if called {
		t.Fatal("fn was called with no items")
	}
}

func TestDoClampsLimit(t *testing.T) {
	var ran int32
	if err := Do(5, 0, func(int) error { atomic.AddInt32(&ran, 1); return nil }); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if ran != 5 {
		t.Fatalf("ran %d times, want 5", ran)
	}
}

func TestDoRecoversPanicInOneLane(t *testing.T) {
	const n = 5
	const poison = 2
	var done [n]int32

	err := Do(n, n, func(i int) error {
		if i == poison {
			panic("bad input")
		}
		atomic.AddInt32(&done[i], 1)
		return nil
	})
	if err == nil {
		t.Fatal("expected an error from the panicking lane")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("worker %d", poison)) {
		t.Fatalf("error %q does not name lane %d", err, poison)
	}
	for i, d := range done {
		want := int32(1)
		if i == poison {
			want = 0
		}
		if d != want {
			t.Fatalf("index %d ran %d times, want %d", i, d, want)
		}
	}
}

func TestDoReturnsFirstErrorByIndex(t *testing.T) {
	err := Do(4, 4, func(i int) error {
		if i == 1 || i == 3 {
			return fmt.Errorf("lane %d", i)
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "lane 1") {
		t.Fatalf("error %q is not from the lowest failing index", err)
	}
}

func TestDoNoItemsIsNoError(t *testing.T) {
	if err := Do(0, 4, func(int) error { t.Fatal("fn called"); return nil }); err != nil {
		t.Fatalf("Do(0) returned %v, want nil", err)
	}
}
