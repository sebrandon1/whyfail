package enrich

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func client(f rtFunc) *http.Client { return &http.Client{Transport: f} }

func respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

func TestSearchCIDefaultsAndCounts(t *testing.T) {
	var gotQuery string
	hc := client(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		gotQuery = q.Get("search") + "|" + q.Get("maxAge")
		if q.Get("maxMatches") != "3" {
			t.Errorf("maxMatches: %q", q.Get("maxMatches"))
		}
		return respond(200, `{
		  "job-a": {"test-a": [{"name":"n","context":["see OCPBUGS-1234 for details"]}]},
		  "job-b": {}
		}`), nil
	})

	res, err := SearchCI(context.Background(), hc, "dial tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "dial tcp|48h" {
		t.Fatalf("query params: %q", gotQuery)
	}
	if res.Query != "dial tcp" || res.Matches != 2 || len(res.Jobs) != 2 {
		t.Fatalf("result: %+v", res)
	}
	if res.KnownBug != "OCPBUGS-1234" {
		t.Fatalf("known bug: %q", res.KnownBug)
	}
}

func TestSearchCIErrors(t *testing.T) {
	bad := client(func(r *http.Request) (*http.Response, error) {
		return respond(503, "busy"), nil
	})
	if _, err := SearchCI(context.Background(), bad, "x", "1h"); err == nil {
		t.Fatal("expected HTTP error")
	}

	junk := client(func(r *http.Request) (*http.Response, error) {
		return respond(200, "not json"), nil
	})
	if _, err := SearchCI(context.Background(), junk, "x", "1h"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestSippyStatus(t *testing.T) {
	cases := []struct {
		name   string
		runID  string
		status int
		body   string
		want   string
	}{
		{"empty run id", "", 200, "", "unknown"},
		{"not tracked", "1", 404, "job run not found", "not tracked"},
		{"tracked", "1", 200, `{"run":{"id":1}}`, "tracked"},
		{"opaque error", "1", 500, "boom", "unknown"},
	}
	for _, c := range cases {
		hc := client(func(r *http.Request) (*http.Response, error) {
			if got := r.URL.Query().Get("prow_job_run_id"); c.runID != "" && got != c.runID {
				t.Errorf("%s: run id param %q", c.name, got)
			}
			return respond(c.status, c.body), nil
		})
		if got := SippyStatus(context.Background(), hc, c.runID); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
