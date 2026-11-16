package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// server answers with the given statuses in order, then 200.
func server(t *testing.T, codes ...int) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(atomic.AddInt32(&calls, 1)) - 1
		code := http.StatusOK
		if i < len(codes) {
			code = codes[i]
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func get(t *testing.T, url string, retries int) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Do(http.DefaultClient, req, retries)
}

func TestDoRetriesThenSucceeds(t *testing.T) {
	srv, calls := server(t, 503, 429, 200)
	resp, err := get(t, srv.URL, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "body" {
		t.Fatalf("body = %q", body)
	}
}

func TestDoReturnsLastResponseWhenRetriesRunOut(t *testing.T) {
	srv, calls := server(t, 503, 503, 503, 503)
	resp, err := get(t, srv.URL, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The caller keeps its existing status handling, so the 503 must survive.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
}

func TestDoDoesNotRetryPermanentStatuses(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusBadRequest} {
		srv, calls := server(t, code)
		resp, err := get(t, srv.URL, 2)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != code || atomic.LoadInt32(calls) != 1 {
			t.Fatalf("%d: status=%d calls=%d", code, resp.StatusCode, atomic.LoadInt32(calls))
		}
	}
}

func TestDoRetriesTransportErrors(t *testing.T) {
	var calls int32
	hc := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return nil, errors.New("connection reset")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test/x", nil)
	resp, err := Do(hc, req, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("status=%d calls=%d", resp.StatusCode, atomic.LoadInt32(&calls))
	}
}

func TestDoWithoutRetriesMakesOneAttempt(t *testing.T) {
	// Zero and negative both mean "do not retry": --retries is a plain int flag.
	for _, retries := range []int{0, -3} {
		srv, calls := server(t, 503)
		resp, err := get(t, srv.URL, retries)
		if err != nil {
			t.Fatalf("retries=%d: %v", retries, err)
		}
		_ = resp.Body.Close()
		if got := atomic.LoadInt32(calls); got != 1 {
			t.Fatalf("retries=%d: server saw %d requests, want 1", retries, got)
		}
	}
}

func TestDoStopsWhenContextEnds(t *testing.T) {
	srv, _ := server(t, 503, 503, 503)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	start := time.Now()
	_, err := Do(http.DefaultClient, req, 2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("returned after %v; the backoff did not observe the context", elapsed)
	}
}

func TestRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		hdr  string
		want time.Duration
		ok   bool
	}{
		{"none", "", 0, false},
		{"seconds", "3", 3 * time.Second, true},
		{"zero", "0", 0, false},
		{"negative", "-1", 0, false},
		{"garbage", "soon", 0, false},
		{"too long", "600", 0, false},
		{"http date", time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat), 5 * time.Second, true},
	}
	for _, c := range cases {
		resp := &http.Response{Header: http.Header{}}
		if c.hdr != "" {
			resp.Header.Set("Retry-After", c.hdr)
		}
		got := retryAfter(resp)
		if c.ok && got < c.want-time.Second {
			t.Errorf("%s: got %v, want about %v", c.name, got, c.want)
		}
		if !c.ok && got != 0 {
			t.Errorf("%s: got %v, want 0", c.name, got)
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	for attempt, lo := range map[int]time.Duration{0: 200 * time.Millisecond, 1: 400 * time.Millisecond, 2: 800 * time.Millisecond} {
		got := backoff(attempt)
		if got < lo || got > lo+100*time.Millisecond {
			t.Errorf("backoff(%d) = %v, want within %v of %v", attempt, got, 100*time.Millisecond, lo)
		}
	}
	if got := backoff(20); got < maxBackoff || got > maxBackoff+100*time.Millisecond {
		t.Errorf("backoff is not capped: %v", got)
	}
}
