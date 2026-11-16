// Package gcs is a minimal, caching client for the public Prow result bucket.
package gcs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sebrandon1/whyfail/internal/httpx"
)

// ErrNotFound is returned when an object does not exist (e.g. a pruned run).
var ErrNotFound = errors.New("object not found")

const (
	listAPI       = "https://storage.googleapis.com/storage/v1/b/%s/o"
	downloadURL   = "https://storage.googleapis.com/%s/%s"
	maxCacheBytes = 64 << 20 // do not cache single objects larger than this
)

// Object is one entry from a bucket listing.
type Object struct {
	Name        string `json:"name"`
	SizeRaw     string `json:"size"`
	ContentType string `json:"contentType,omitempty"`
	Updated     string `json:"updated"`

	Size int64 `json:"-"`
}

// Client fetches and caches immutable Prow artifacts.
type Client struct {
	HTTP     *http.Client
	CacheDir string
	Refresh  bool
	// Retries is how many times a transient failure is retried, so a command
	// makes at most Retries+1 attempts per request.
	Retries int

	// NetworkBytes counts decompressed bytes downloaded from GCS.
	NetworkBytes int64
	// ServedBytes counts bytes made available to callers, including cache hits.
	ServedBytes int64
	Fetched     int64
	CacheHits   int64
	Missing     int64
}

// New creates a client rooted at cacheDir.
func New(cacheDir string) *Client {
	return &Client{
		CacheDir: cacheDir,
		Retries:  2,
		HTTP: &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     60 * time.Second,
				// Leave Accept-Encoding unset so net/http negotiates and
				// transparently decompresses gzip objects.
				ForceAttemptHTTP2: true,
			},
		},
	}
}

func (c *Client) cachePath(bucket, object string) string {
	sum := sha256.Sum256([]byte(bucket + "\n" + object))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(c.CacheDir, "objects", h[:2], h)
}

// pageResult is the subset of a GCS listing response we consume.
type pageResult struct {
	Items         []Object `json:"items"`
	Prefixes      []string `json:"prefixes"`
	NextPageToken string   `json:"nextPageToken"`
}

// fetchPage performs one listing request.
func (c *Client) fetchPage(ctx context.Context, bucket string, q url.Values) (pageResult, error) {
	u := fmt.Sprintf(listAPI, url.PathEscape(bucket)) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return pageResult{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := httpx.Do(c.HTTP, req, c.Retries)
	if err != nil {
		return pageResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return pageResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return pageResult{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var pr pageResult
	if err := json.Unmarshal(body, &pr); err != nil {
		return pageResult{}, fmt.Errorf("decode listing: %w", err)
	}
	return pr, nil
}

// walkPages follows page tokens until the listing is exhausted.
func (c *Client) walkPages(ctx context.Context, bucket string, buildQuery func(pageToken string) url.Values, onPage func(pageResult)) error {
	page := ""
	for {
		pr, err := c.fetchPage(ctx, bucket, buildQuery(page))
		if err != nil {
			return err
		}
		onPage(pr)
		if pr.NextPageToken == "" {
			return nil
		}
		page = pr.NextPageToken
	}
}

// List returns every object under prefix, following pagination.
func (c *Client) List(ctx context.Context, bucket, prefix string) ([]Object, error) {
	var out []Object
	err := c.walkPages(ctx, bucket, func(page string) url.Values {
		q := url.Values{}
		q.Set("prefix", prefix)
		q.Set("fields", "items(name,size,updated),nextPageToken")
		q.Set("maxResults", "1000")
		if page != "" {
			q.Set("pageToken", page)
		}
		return q
	}, func(pr pageResult) {
		for i := range pr.Items {
			pr.Items[i].Size, _ = strconv.ParseInt(pr.Items[i].SizeRaw, 10, 64)
			out = append(out, pr.Items[i])
		}
	})
	if err != nil {
		return nil, fmt.Errorf("list %s/%s: %w", bucket, prefix, err)
	}
	return out, nil
}

// ListLimit returns at most max objects from the first page of a listing.
func (c *Client) ListLimit(ctx context.Context, bucket, prefix string, max int) ([]Object, error) {
	q := url.Values{}
	q.Set("prefix", prefix)
	q.Set("fields", "items(name,size,updated)")
	if max > 0 {
		q.Set("maxResults", strconv.Itoa(max))
	}
	pr, err := c.fetchPage(ctx, bucket, q)
	if err != nil {
		return nil, fmt.Errorf("list %s/%s: %w", bucket, prefix, err)
	}
	for i := range pr.Items {
		pr.Items[i].Size, _ = strconv.ParseInt(pr.Items[i].SizeRaw, 10, 64)
	}
	return pr.Items, nil
}

// Stat returns metadata for a single object.
func (c *Client) Stat(ctx context.Context, bucket, object string) (Object, error) {
	u := fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o/%s?fields=name,size,contentType,updated",
		url.PathEscape(bucket), url.PathEscape(object))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Object{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := httpx.Do(c.HTTP, req, c.Retries)
	if err != nil {
		return Object{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return Object{}, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return Object{}, fmt.Errorf("stat %s: HTTP %d: %s", object, resp.StatusCode, snippet(body))
	}
	var o Object
	if err := json.Unmarshal(body, &o); err != nil {
		return Object{}, err
	}
	o.Size, _ = strconv.ParseInt(o.SizeRaw, 10, 64)
	return o, nil
}

// ListDirs returns the immediate subdirectory prefixes under prefix.
func (c *Client) ListDirs(ctx context.Context, bucket, prefix string) ([]string, error) {
	var out []string
	err := c.walkPages(ctx, bucket, func(page string) url.Values {
		q := url.Values{}
		q.Set("prefix", prefix)
		q.Set("delimiter", "/")
		q.Set("fields", "prefixes,nextPageToken")
		q.Set("maxResults", "1000")
		if page != "" {
			q.Set("pageToken", page)
		}
		return q
	}, func(pr pageResult) {
		out = append(out, pr.Prefixes...)
	})
	if err != nil {
		return nil, fmt.Errorf("list dirs %s/%s: %w", bucket, prefix, err)
	}
	return out, nil
}

// Get returns the full contents of an object, using the on-disk cache.
func (c *Client) Get(ctx context.Context, bucket, object string) ([]byte, error) {
	p := c.cachePath(bucket, object)
	if !c.Refresh {
		if data, err := os.ReadFile(p); err == nil {
			atomic.AddInt64(&c.CacheHits, 1)
			atomic.AddInt64(&c.ServedBytes, int64(len(data)))
			return data, nil
		}
	}

	data, err := c.fetch(ctx, bucket, object, -1)
	if err != nil {
		return nil, err
	}
	if len(data) <= maxCacheBytes {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		tmp := p + ".tmp"
		if werr := os.WriteFile(tmp, data, 0o644); werr == nil {
			_ = os.Rename(tmp, p)
		}
	}
	atomic.AddInt64(&c.ServedBytes, int64(len(data)))
	return data, nil
}

// GetLimit returns at most max bytes of an object without caching the result.
func (c *Client) GetLimit(ctx context.Context, bucket, object string, max int64) ([]byte, error) {
	data, err := c.fetch(ctx, bucket, object, max)
	if err != nil {
		return nil, err
	}
	atomic.AddInt64(&c.ServedBytes, int64(len(data)))
	return data, nil
}

func (c *Client) fetch(ctx context.Context, bucket, object string, limit int64) ([]byte, error) {
	u := fmt.Sprintf(downloadURL, url.PathEscape(bucket), escapePath(object))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := httpx.Do(c.HTTP, req, c.Retries)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", object, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		atomic.AddInt64(&c.Missing, 1)
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("get %s: HTTP %d: %s", object, resp.StatusCode, snippet(body))
	}

	var r io.Reader = resp.Body
	if limit > 0 {
		r = io.LimitReader(resp.Body, limit)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	atomic.AddInt64(&c.Fetched, 1)
	atomic.AddInt64(&c.NetworkBytes, int64(len(data)))
	return data, nil
}

func escapePath(object string) string {
	// Escape each segment but keep the separators, so spaces and punctuation
	// in artifact names are handled without breaking the path structure.
	return (&url.URL{Path: "/" + object}).EscapedPath()[1:]
}

func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// UserAgent identifies the tool to the CI infrastructure operators.
var UserAgent = "whyfail/dev (+https://github.com/sebrandon1/whyfail)"
