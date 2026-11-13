package gcstest

import (
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCorpusStoredSizes checks that each committed job body is exactly the size
// the listing reports. The listing's `size` is the *stored* (gzip) size, which
// is what Artifacts.TotalBytes is computed from; committing a decompressed body
// would quietly inflate the inventory and make `gain` lie.
func TestCorpusStoredSizes(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(RepoRoot(t), "testdata", "*", "jobs", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 4 {
		t.Fatalf("expected at least four job corpora, found %d", len(dirs))
	}

	for _, dir := range dirs {
		d := Load(t, dir)
		sizes := make(map[string]string, len(d.Listing))
		for _, it := range d.Listing {
			sizes[it.Name] = it.Size
		}

		objRoot := filepath.Join(dir, "objects")
		checked := 0
		err := filepath.WalkDir(objRoot, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, err := filepath.Rel(objRoot, path)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)
			want, ok := sizes[name]
			if !ok {
				t.Errorf("%s: object %q is not in the listing", filepath.Base(dir), name)
				return nil
			}
			fi, err := e.Info()
			if err != nil {
				return err
			}
			if got := strconv.FormatInt(fi.Size(), 10); got != want {
				t.Errorf("%s: %q is %s bytes on disk but the listing says %s stored; "+
					"a gzip object must be committed in its stored form",
					filepath.Base(dir), name, got, want)
			}
			checked++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if checked == 0 {
			t.Fatalf("%s: no objects", filepath.Base(dir))
		}
	}
}

// TestCorpusHealthIndex sanity-checks the recorded presubmit index.
func TestCorpusHealthIndex(t *testing.T) {
	d := Load(t, filepath.Join(RepoRoot(t), "testdata", "recert-2364", "health",
		"pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow"))

	if len(d.Listing) != 140 {
		t.Errorf("index listing has %d entries, want 140", len(d.Listing))
	}
	pointers, finished := 0, 0
	for name := range d.Objects {
		switch {
		case strings.HasSuffix(name, ".txt"):
			pointers++
		case strings.HasSuffix(name, "/finished.json"):
			finished++
		}
	}
	// latest-build.txt is deliberately not recorded: it holds a bare run id,
	// not a gs:// pointer, and health.enumerate skips it.
	if pointers != 139 {
		t.Errorf("recorded %d .txt pointers, want 139", pointers)
	}
	if finished != 139 {
		t.Errorf("recorded %d finished.json files, want 139", finished)
	}
}

// TestCorpusListingSizes guards the fake transport's fidelity: the GCS API
// returns `size` as a quoted string, and gcs.Client parses it as one.
func TestCorpusListingSizes(t *testing.T) {
	d := Load(t, filepath.Join(RepoRoot(t), "testdata", "recert-2364", "jobs", "ipc"))
	if len(d.Listing) != 72 {
		t.Errorf("ipc listing has %d items, want 72", len(d.Listing))
	}
	var total int64
	for _, it := range d.Listing {
		n, err := strconv.ParseInt(it.Size, 10, 64)
		if err != nil {
			t.Errorf("%q: size %q is not a number", it.Name, it.Size)
			continue
		}
		total += n
	}
	if total != 24052955 {
		t.Errorf("ipc inventory = %d, want 24052955", total)
	}
}
