package gcstest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// transport answers GCS listing, stat and download requests from a Dataset.
type transport struct {
	dataset *Dataset
}

var _ http.RoundTripper = (*transport)(nil)

// RoundTrip routes only to storage.googleapis.com; any other host fails
// loudly, so an accidental real call is caught rather than made.
func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "storage.googleapis.com" {
		return nil, fmt.Errorf("unexpected host %q: the corpus only serves storage.googleapis.com", r.URL.Host)
	}
	switch {
	case r.URL.Path == "/storage/v1/b/"+Bucket+"/o":
		return t.list(r), nil
	case strings.HasPrefix(r.URL.Path, "/storage/v1/b/"+Bucket+"/o/"):
		return t.stat(r), nil
	case strings.HasPrefix(r.URL.Path, "/"+Bucket+"/"):
		return t.object(strings.TrimPrefix(r.URL.Path, "/"+Bucket+"/")), nil
	default:
		return notFound(), nil
	}
}

func (t *transport) list(r *http.Request) *http.Response {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	t.dataset.record("list " + prefix)

	var body []byte
	if q.Get("delimiter") == "/" {
		body, _ = json.Marshal(map[string]any{"prefixes": prefixesOf(t.dataset.Listing, prefix)})
	} else {
		items := make([]Item, 0, len(t.dataset.Listing))
		for _, it := range t.dataset.Listing {
			if strings.HasPrefix(it.Name, prefix) {
				items = append(items, it)
			}
		}
		body, _ = json.Marshal(map[string]any{"items": items})
	}
	return ok(body)
}

func (t *transport) stat(r *http.Request) *http.Response {
	name, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/storage/v1/b/"+Bucket+"/o/"))
	if err != nil {
		return notFound()
	}
	t.dataset.record("stat " + name)
	for _, it := range t.dataset.Listing {
		if it.Name == name {
			body, _ := json.Marshal(it)
			return ok(body)
		}
	}
	return notFound()
}

func (t *transport) object(name string) *http.Response {
	decoded, err := url.PathUnescape(name)
	if err != nil {
		return notFound()
	}
	t.dataset.record("get " + decoded)
	data, found := t.dataset.Objects[decoded]
	if !found {
		return notFound()
	}
	// The dataset holds decompressed bytes, so serve them plainly: this is a
	// RoundTripper, and http.Transport's automatic gzip handling does not run.
	return ok(data)
}

func ok(body []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{},
	}
}

func notFound() *http.Response {
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("no such object")),
		Header:     http.Header{},
	}
}
