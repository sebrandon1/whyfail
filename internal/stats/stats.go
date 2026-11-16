// Package stats records whyfail invocations and aggregates the savings shown
// by `whyfail gain`.
package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Record is one invocation.
type Record struct {
	TS             time.Time `json:"ts"`
	Cmd            string    `json:"cmd"`
	Repo           string    `json:"repo,omitempty"`
	Target         string    `json:"target,omitempty"`
	InventoryBytes int64     `json:"inventory_bytes"`
	ReadBytes      int64     `json:"read_bytes"`
	NetworkBytes   int64     `json:"network_bytes"`
	EmittedBytes   int64     `json:"emitted_bytes"`
	CacheHits      int64     `json:"cache_hits"`
	Missing        int64     `json:"missing"`
	WallMS         int64     `json:"wall_ms"`
	Verdict        string    `json:"verdict,omitempty"`
}

// Dir returns the per-user cache directory.
func Dir() string {
	if d := os.Getenv("WHYFAIL_CACHE_DIR"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "whyfail")
	}
	return filepath.Join(os.TempDir(), "whyfail")
}

// Path returns the stats file path.
func Path() string { return filepath.Join(Dir(), "stats.jsonl") }

// Enabled reports whether recording is on.
func Enabled() bool { return os.Getenv("WHYFAIL_NO_STATS") == "" }

const maxFileBytes = 5 << 20

// Append appends one invocation record.
func Append(r Record) error {
	if !Enabled() {
		return nil
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	p := Path()
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxFileBytes {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = f.Write(b)
	return err
}

// Load reads every valid record, skipping corrupt lines. The active file and
// its rotated predecessor are both read, oldest first, so totals and
// `--since` windows survive rotation instead of resetting.
func Load() ([]Record, error) {
	var out []Record
	for _, p := range []string{Path() + ".1", Path()} {
		recs, err := loadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return dedupe(out), nil
}

// loadFile reads one stats file. A missing file is not an error.
func loadFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// recordKey identifies one invocation. Two records with the same timestamp,
// command and target are the same call, whether a rotation or a crash left a
// copy behind.
type recordKey struct {
	ts     time.Time
	cmd    string
	target string
}

// dedupe drops repeated records while preserving order.
func dedupe(recs []Record) []Record {
	if len(recs) < 2 {
		return recs
	}
	seen := make(map[recordKey]struct{}, len(recs))
	out := recs[:0]
	for _, r := range recs {
		k := recordKey{ts: r.TS, cmd: r.Cmd, target: r.Target}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// Totals are summed byte and call counts.
type Totals struct {
	Calls          int   `json:"calls"`
	InventoryBytes int64 `json:"inventory_bytes"`
	ReadBytes      int64 `json:"read_bytes"`
	NetworkBytes   int64 `json:"network_bytes"`
	EmittedBytes   int64 `json:"emitted_bytes"`
	CacheHits      int64 `json:"cache_hits"`
	Missing        int64 `json:"missing"`
	WallMS         int64 `json:"wall_ms"`
}

// Add accumulates one record.
func (t *Totals) Add(r Record) {
	t.Calls++
	t.InventoryBytes += r.InventoryBytes
	t.ReadBytes += r.ReadBytes
	t.NetworkBytes += r.NetworkBytes
	t.EmittedBytes += r.EmittedBytes
	t.CacheHits += r.CacheHits
	t.Missing += r.Missing
	t.WallMS += r.WallMS
}

// Aggregate is the result of `whyfail gain`.
type Aggregate struct {
	Since  time.Time         `json:"since"`
	Until  time.Time         `json:"until"`
	Totals Totals            `json:"totals"`
	Groups map[string]Totals `json:"groups,omitempty"`
	Top    []Record          `json:"top_saves,omitempty"`
}

// Summarize sums records newer than since. by may be "cmd", "repo" or empty.
func Summarize(recs []Record, by string, since time.Time) Aggregate {
	a := Aggregate{Groups: map[string]Totals{}}
	for _, r := range recs {
		if !since.IsZero() && r.TS.Before(since) {
			continue
		}
		if a.Since.IsZero() || r.TS.Before(a.Since) {
			a.Since = r.TS
		}
		if r.TS.After(a.Until) {
			a.Until = r.TS
		}
		a.Totals.Add(r)
		if by != "" {
			key := r.Cmd
			if by == "repo" {
				key = r.Repo
			}
			t := a.Groups[key]
			t.Add(r)
			a.Groups[key] = t
		}
	}
	sorted := append([]Record(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool {
		return saved(sorted[i]) > saved(sorted[j])
	})
	for i := 0; i < len(sorted) && i < 5; i++ {
		if saved(sorted[i]) <= 0 {
			break
		}
		a.Top = append(a.Top, sorted[i])
	}
	return a
}

func saved(r Record) int64 { return r.ReadBytes - r.EmittedBytes }
