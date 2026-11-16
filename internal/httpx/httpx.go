// Package httpx performs GET requests with bounded retries for the transient
// failures the public CI services produce under load.
package httpx

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	baseBackoff = 200 * time.Millisecond
	maxBackoff  = 2 * time.Second
	// maxRetryAfter caps a server-supplied wait. Honouring a two-minute
	// Retry-After would stall an interactive command.
	maxRetryAfter = 10 * time.Second
	// drainLimit is how much of a discarded body is read before closing, so the
	// connection can be reused without downloading an error page in full.
	drainLimit = 4 << 10
)

// Do performs req and retries transient failures up to retries times, so at
// most retries+1 attempts are made. The caller keeps ownership of the returned
// body. On the final attempt the response and error are returned unchanged, so
// existing status handling in callers is preserved.
//
// req must have no body: the same request is re-sent on each attempt.
func Do(hc *http.Client, req *http.Request, retries int) (*http.Response, error) {
	if retries < 0 {
		retries = 0
	}
	attempts := retries + 1

	for attempt := 0; ; attempt++ {
		resp, err := hc.Do(req)
		last := attempt+1 >= attempts
		if last {
			return resp, err
		}

		if err != nil {
			// A cancelled or expired context is not worth another attempt.
			if req.Context().Err() != nil {
				return nil, err
			}
			if werr := wait(req.Context(), backoff(attempt)); werr != nil {
				return nil, err
			}
			continue
		}

		if !retryableStatus(resp.StatusCode) {
			return resp, nil
		}

		delay := retryAfter(resp)
		if delay == 0 {
			delay = backoff(attempt)
		}
		drain(resp)
		if werr := wait(req.Context(), delay); werr != nil {
			return nil, werr
		}
	}
}

// retryableStatus reports whether a status is a transient condition.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfter returns the wait the server asked for, or 0 when it asked for none
// or asked for more than maxRetryAfter.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d <= 0 || d > maxRetryAfter {
		return 0
	}
	return d
}

// drain reads and closes a body that will not be used, keeping the connection
// in the pool.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}

// backoff grows 200ms, 400ms, 800ms… capped at maxBackoff, with up to 100ms of
// jitter so retries from parallel requests do not align.
func backoff(attempt int) time.Duration {
	d := baseBackoff << attempt
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	return d + time.Duration(rand.Int64N(int64(100*time.Millisecond)+1))
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
