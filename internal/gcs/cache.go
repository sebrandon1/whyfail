package gcs

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// cacheEntry is one cached object on disk. Cached objects are write-once, so
// mtime is the time it was fetched — a good proxy for freshness, unlike atime,
// which APFS does not reliably maintain.
type cacheEntry struct {
	path string
	size int64
	mod  time.Time
}

// walkCache lists every cached object. A missing cache directory is empty, not
// an error, and an entry that cannot be read is skipped rather than failing the
// command.
func (c *Client) walkCache() ([]cacheEntry, error) {
	root := filepath.Join(c.CacheDir, "objects")
	var out []cacheEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out = append(out, cacheEntry{path: p, size: info.Size(), mod: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CacheSize reports how much the object cache holds.
func (c *Client) CacheSize() (bytes int64, files int, err error) {
	entries, err := c.walkCache()
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		bytes += e.size
	}
	return bytes, len(entries), nil
}

// Prune keeps the object cache at or under maxBytes by evicting the oldest
// objects, never touching one younger than minAge so a command cannot lose a
// file it just wrote. Age is therefore the eviction order rather than a
// separate TTL: a cache under the limit is left alone however old it is, and a
// cache made entirely of fresh objects stays over the limit until they age.
//
// maxBytes <= 0 disables pruning. Pruning is best effort: it reports what it
// removed and a failure to remove one object does not stop the rest.
func (c *Client) Prune(maxBytes int64, minAge time.Duration) (removed int, freed int64, err error) {
	if maxBytes <= 0 {
		return 0, 0, nil
	}
	entries, err := c.walkCache()
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.size
	}
	if total <= maxBytes {
		return 0, 0, nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.Before(entries[j].mod) })
	now := time.Now()
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		if minAge > 0 && now.Sub(e.mod) < minAge {
			continue
		}
		if rmErr := os.Remove(e.path); rmErr == nil {
			total -= e.size
			removed++
			freed += e.size
		}
	}
	return removed, freed, nil
}
