// Package gcstest serves a recorded Prow/GCS corpus through a gcs.Client so
// tests can exercise the real build paths without network access.
package gcstest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sebrandon1/whyfail/internal/gcs"
)

// Bucket is the public Prow results bucket the corpus was captured from.
const Bucket = "test-platform-results-public"

// Item is one entry of a captured listing. Size is a string because that is
// what the GCS JSON API returns and what gcs.Client parses.
type Item struct {
	Name    string `json:"name"`
	Size    string `json:"size"`
	Updated string `json:"updated"`
}

// Dataset is a captured object tree plus the listing that described it.
type Dataset struct {
	// Objects maps a full GCS object name to the bytes a client would receive
	// (gzip bodies are decompressed on load).
	Objects map[string][]byte
	// Listing is every object under the captured prefix, with stored sizes.
	Listing []Item

	mu       sync.Mutex
	requests []string
}

// Requests returns the object paths the client asked for, in order.
func (d *Dataset) Requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func (d *Dataset) record(path string) {
	d.mu.Lock()
	d.requests = append(d.requests, path)
	d.mu.Unlock()
}

// RunPrefix returns the GCS path prefix of the run the dataset was captured
// for, i.e. everything before the run's prowjob.json.
func (d *Dataset) RunPrefix() string {
	for _, it := range d.Listing {
		if strings.HasSuffix(it.Name, "/prowjob.json") {
			return strings.TrimSuffix(it.Name, "/prowjob.json")
		}
	}
	return ""
}

// RepoRoot returns the repository root, derived from this file's location.
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// Load reads a captured dataset from dir, which must contain listing.json.gz
// and an objects/ tree mirroring the GCS object names.
func Load(t *testing.T, dir string) *Dataset {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "listing.json.gz"))
	if err != nil {
		t.Fatalf("read listing: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open listing: %v", err)
	}
	var page struct {
		Items []Item `json:"items"`
	}
	if err := json.NewDecoder(zr).Decode(&page); err != nil {
		t.Fatalf("decode listing: %v", err)
	}

	d := &Dataset{Objects: map[string][]byte{}, Listing: page.Items}
	root := filepath.Join(dir, "objects")
	err = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isGzip(body) {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			body, err = io.ReadAll(zr)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		d.Objects[name] = body
		return nil
	})
	if err != nil {
		t.Fatalf("walk objects: %v", err)
	}
	if len(d.Objects) == 0 {
		t.Fatalf("no objects under %s", root)
	}
	return d
}

func isGzip(b []byte) bool { return len(b) > 1 && b[0] == 0x1f && b[1] == 0x8b }

// Client returns a gcs.Client backed by the dataset. Its cache lives in a
// temporary directory, so cold reads hit the dataset and warm reads hit disk.
func Client(t *testing.T, d *Dataset) *gcs.Client {
	t.Helper()
	return newClient(t.TempDir(), d)
}

func newClient(cacheDir string, d *Dataset) *gcs.Client {
	c := gcs.New(cacheDir)
	c.HTTP = &http.Client{Transport: &transport{dataset: d}}
	return c
}

// prefixesOf returns the immediate subdirectory prefixes of prefix.
func prefixesOf(items []Item, prefix string) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if !strings.HasPrefix(it.Name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(it.Name, prefix)
		if i := strings.Index(rest, "/"); i > 0 {
			p := prefix + rest[:i+1]
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
