package gcs

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

func newTestClient(t *testing.T, f rtFunc) *Client {
	t.Helper()
	c := New(t.TempDir())
	c.HTTP = &http.Client{Transport: f}
	return c
}

func TestListPaginates(t *testing.T) {
	var tokens []string
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		tok := r.URL.Query().Get("pageToken")
		tokens = append(tokens, tok)
		if tok == "" {
			return respond(200, `{"items":[{"name":"a","size":"10","updated":"u"}],"nextPageToken":"p2"}`), nil
		}
		return respond(200, `{"items":[{"name":"b","size":"20","updated":"u"}]}`), nil
	})

	objs, err := c.List(context.Background(), "bkt", "pre/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Name != "a" || objs[1].Name != "b" {
		t.Fatalf("objects: %+v", objs)
	}
	if objs[0].Size != 10 || objs[1].Size != 20 {
		t.Fatalf("sizes: %+v", objs)
	}
	if len(tokens) != 2 || tokens[0] != "" || tokens[1] != "p2" {
		t.Fatalf("page tokens: %v", tokens)
	}
}

func TestListDirs(t *testing.T) {
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("delimiter") != "/" {
			t.Errorf("missing delimiter: %s", r.URL)
		}
		if r.URL.Query().Get("pageToken") == "" {
			return respond(200, `{"prefixes":["p/1/"],"nextPageToken":"n"}`), nil
		}
		return respond(200, `{"prefixes":["p/2/"]}`), nil
	})
	dirs, err := c.ListDirs(context.Background(), "bkt", "p/")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 || dirs[1] != "p/2/" {
		t.Fatalf("dirs: %v", dirs)
	}
}

func TestListLimitIsSinglePage(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if got := r.URL.Query().Get("maxResults"); got != "5" {
			t.Errorf("maxResults: %q", got)
		}
		return respond(200, `{"items":[{"name":"a","size":"7"}],"nextPageToken":"ignored"}`), nil
	})
	objs, err := c.ListLimit(context.Background(), "bkt", "pre/", 5)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(objs) != 1 || objs[0].Size != 7 {
		t.Fatalf("calls=%d objects=%+v", calls, objs)
	}
}

func TestStat(t *testing.T) {
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "missing") {
			return respond(404, `{}`), nil
		}
		return respond(200, `{"name":"x","size":"42","updated":"u"}`), nil
	})
	o, err := c.Stat(context.Background(), "bkt", "x")
	if err != nil || o.Size != 42 {
		t.Fatalf("stat: %+v %v", o, err)
	}
	if _, err := c.Stat(context.Background(), "bkt", "missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGetCachesAndRefreshBypasses(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		requests++
		return respond(200, "payload"), nil
	})

	for i := 0; i < 2; i++ {
		data, err := c.Get(context.Background(), "bkt", "obj")
		if err != nil || string(data) != "payload" {
			t.Fatalf("get %d: %q %v", i, data, err)
		}
	}
	if requests != 1 {
		t.Fatalf("expected one network fetch, got %d", requests)
	}
	if c.CacheHits != 1 || c.Fetched != 1 {
		t.Fatalf("cache hits=%d fetched=%d", c.CacheHits, c.Fetched)
	}
	if c.ServedBytes != 14 {
		t.Fatalf("served bytes: %d", c.ServedBytes)
	}

	c.Refresh = true
	if _, err := c.Get(context.Background(), "bkt", "obj"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("refresh should refetch, requests=%d", requests)
	}
}

func TestGetNotFound(t *testing.T) {
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		return respond(404, "nope"), nil
	})
	if _, err := c.Get(context.Background(), "bkt", "obj"); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
	if c.Missing != 1 {
		t.Fatalf("missing count: %d", c.Missing)
	}
}

func TestGetLimitDoesNotCache(t *testing.T) {
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		return respond(200, "abcdefghij"), nil
	})
	data, err := c.GetLimit(context.Background(), "bkt", "obj", 5)
	if err != nil || string(data) != "abcde" {
		t.Fatalf("got %q %v", data, err)
	}
	if _, err := os.Stat(c.cachePath("bkt", "obj")); !os.IsNotExist(err) {
		t.Fatalf("GetLimit should not cache, stat err=%v", err)
	}
}

func TestFetchErrorIncludesSnippet(t *testing.T) {
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		return respond(500, "server exploded"), nil
	})
	_, err := c.Get(context.Background(), "bkt", "obj")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "server exploded") {
		t.Fatalf("got %v", err)
	}
}

func TestEscapePath(t *testing.T) {
	if got := escapePath("a b/c d.txt"); got != "a%20b/c%20d.txt" {
		t.Fatalf("got %q", got)
	}
}

func TestGetRetriesTransientFailure(t *testing.T) {
	var calls int
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return respond(503, "busy"), nil
		}
		return respond(200, "payload"), nil
	})

	data, err := c.Get(context.Background(), "bkt", "obj")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" || calls != 3 {
		t.Fatalf("data=%q calls=%d", data, calls)
	}
	// The bytes of a discarded attempt must not be counted.
	if c.NetworkBytes != int64(len("payload")) {
		t.Fatalf("network bytes = %d, want %d", c.NetworkBytes, len("payload"))
	}
}

func TestGetHonoursRetryLimit(t *testing.T) {
	var calls int
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return respond(503, "busy"), nil
	})
	c.Retries = 1

	if _, err := c.Get(context.Background(), "bkt", "obj"); err == nil {
		t.Fatal("expected an error after the retries ran out")
	}
	if calls != 2 {
		t.Fatalf("made %d requests, want 2", calls)
	}
}

func TestListRetriesTransientFailure(t *testing.T) {
	var calls int
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls < 2 {
			return respond(429, "slow down"), nil
		}
		return respond(200, `{"items":[{"name":"a","size":"10","updated":"u"}]}`), nil
	})

	objs, err := c.List(context.Background(), "bkt", "pre/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || calls != 2 {
		t.Fatalf("objects=%d calls=%d", len(objs), calls)
	}
}
