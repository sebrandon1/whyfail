package gcs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedCache writes n objects of size bytes, then sets their mtime to age ago.
func seedCache(t *testing.T, c *Client, prefix string, n int, size int64, age time.Duration) []string {
	t.Helper()
	var paths []string
	when := time.Now().Add(-age)
	for i := 0; i < n; i++ {
		p := filepath.Join(c.CacheDir, "objects", "aa", fmt.Sprintf("%s-%d", prefix, i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

func TestPruneOldestFirst(t *testing.T) {
	c := New(t.TempDir())
	old := seedCache(t, c, "old", 3, 1000, 2*time.Hour)
	fresh := seedCache(t, c, "new", 1, 1000, 0)

	removed, freed, err := c.Prune(2000, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 || freed != 2000 {
		t.Fatalf("removed=%d freed=%d", removed, freed)
	}
	// All three old objects share an mtime, so which two go is arbitrary; what
	// matters is that two went and the fresh one stayed.
	kept := 0
	for _, p := range old {
		if _, err := os.Stat(p); err == nil {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("%d old objects survived, want 1", kept)
	}
	if _, err := os.Stat(fresh[0]); err != nil {
		t.Errorf("fresh object was pruned: %v", err)
	}
}

func TestPruneBySize(t *testing.T) {
	c := New(t.TempDir())
	// minAge 0 disables the freshness floor, so the size limit alone decides.
	seedCache(t, c, "obj", 10, 1000, 2*time.Hour)

	if _, _, err := c.Prune(3000, 0); err != nil {
		t.Fatal(err)
	}
	size, files, err := c.CacheSize()
	if err != nil {
		t.Fatal(err)
	}
	if size > 3000 {
		t.Fatalf("cache is %d bytes, want <= 3000", size)
	}
	if files != 3 {
		t.Fatalf("files = %d, want 3", files)
	}
}

func TestPruneUnderLimitIsNoOp(t *testing.T) {
	c := New(t.TempDir())
	seedCache(t, c, "obj", 4, 100, 2*time.Hour)

	// Under the limit, age alone is not a reason to evict: the cache stays warm.
	removed, _, err := c.Prune(1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if _, files, _ := c.CacheSize(); files != 4 {
		t.Fatalf("files = %d, want 4", files)
	}
}

func TestPruneKeepsFreshObjectsOverLimit(t *testing.T) {
	// Objects younger than minAge are never removed, so a cache of fresh
	// objects can stay over the limit rather than losing a file a command is
	// still using.
	c := New(t.TempDir())
	seedCache(t, c, "obj", 5, 1000, 0)

	removed, _, err := c.Prune(1000, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
}

func TestCacheSizeEmpty(t *testing.T) {
	c := New(t.TempDir())
	size, files, err := c.CacheSize()
	if err != nil {
		t.Fatalf("missing cache dir must not error: %v", err)
	}
	if size != 0 || files != 0 {
		t.Fatalf("size=%d files=%d", size, files)
	}
}

func TestPruneEmptyCache(t *testing.T) {
	c := New(t.TempDir())
	if _, _, err := c.Prune(1<<20, time.Hour); err != nil {
		t.Fatalf("prune on an empty cache: %v", err)
	}
}
