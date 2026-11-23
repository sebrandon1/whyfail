// Package health computes job-level pass rates from Prow's retained run index.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sebrandon1/whyfail/internal/api"
	"github.com/sebrandon1/whyfail/internal/gcs"
	"github.com/sebrandon1/whyfail/internal/parallel"
	"github.com/sebrandon1/whyfail/internal/prow"
)

// ErrJobNotFound is returned when a job name matches neither the presubmit
// index nor the periodic log layout. That is almost always a typo or a
// renamed job, and it is worth saying so rather than printing an empty card.
var ErrJobNotFound = errors.New("job not found")

// fanOut bounds concurrent requests to the public bucket. A wide window on a
// busy presubmit is a few hundred runs, and the bucket rate-limits.
const fanOut = 16

// pruneGrace is how much older than the window an index entry may be and still
// be fetched. The index `updated` field is when the run was recorded, while the
// window is applied to the finished timestamp, so a long job that started just
// before the cutoff can still finish inside it. 12h covers the 8h timeouts
// these jobs use, plus queueing.
const pruneGrace = 12 * time.Hour

// RunResult is one historical run of a job.
type RunResult struct {
	ID     string    `json:"id"`
	Path   string    `json:"path"`
	Result string    `json:"result"` // SUCCESS, FAILURE, ABORTED, MISSING
	Time   time.Time `json:"time"`
}

// Verdict is the health classification derived from retained history.
type Verdict string

const (
	VerdictUnknown  Verdict = "UNKNOWN"
	VerdictBroken   Verdict = "BROKEN"
	VerdictFlaky    Verdict = "FLAKY"
	VerdictDegraded Verdict = "DEGRADED"
	VerdictHealthy  Verdict = "HEALTHY"
)

// IsBad reports whether the job warrants intervention before a retest.
func (v Verdict) IsBad() bool {
	return v == VerdictBroken || v == VerdictFlaky
}

// Summary describes a job's history over the retained window.
type Summary struct {
	Schema         string     `json:"schema"`
	Job            string     `json:"job"`
	From           time.Time  `json:"from"`
	To             time.Time  `json:"to"`
	Runs           int        `json:"runs"`
	Pass           int        `json:"pass"`
	Fail           int        `json:"fail"`
	Abort          int        `json:"abort"`
	Missing        int        `json:"missing"`
	PassRate       float64    `json:"pass_rate"`
	RecentPassRate float64    `json:"recent_pass_rate"`
	Streak         int        `json:"streak"`
	LastPass       *time.Time `json:"last_pass_at,omitempty"`
	DistinctPRs    int        `json:"distinct_prs_failing"`
	// FailingPRs names the PRs behind DistinctPRs, sorted. It is empty for a
	// healthy job and for a periodic one, whose runs have no PR path.
	FailingPRs []string    `json:"failing_prs,omitempty"`
	Verdict    Verdict     `json:"verdict"`
	Samples    []RunResult `json:"-"`
}

// Repo names the repository this job ran for, from the first sample that came
// from a pull-request path. A periodic job has none, which is not an error: the
// path is the only offline source, so nothing is guessed. prow.Repo owns the
// org_repo split (the org never contains an underscore), so this does not
// re-implement it.
func (s *Summary) Repo() string {
	for _, r := range s.Samples {
		if repo := prow.Repo(r.Path); repo != "" {
			return repo
		}
	}
	return ""
}

// Collect enumerates a job's runs and summarizes them. window <= 0 means the
// full retained history.
func Collect(ctx context.Context, c *gcs.Client, bucket, job string, window time.Duration) (*Summary, error) {
	paths, err := enumerate(ctx, c, bucket, job, window)
	if err != nil {
		return nil, err
	}
	// The index pre-filter already dropped entries outside the window; this
	// stays as the safety net for layouts whose listing carried no timestamp.
	paths = recentOnly(paths, window)

	// Each goroutine writes its own slot, so the results are ordered by run
	// rather than by completion.
	results := make([]RunResult, len(paths))
	err = parallel.Do(len(paths), fanOut, func(i int) error {
		p := paths[i]
		rr := RunResult{ID: p.ID, Path: p.Path, Time: p.Updated}
		data, err := c.Get(ctx, bucket, p.Path+"/finished.json")
		if err == nil {
			var f struct {
				Result    string `json:"result"`
				Timestamp int64  `json:"timestamp"`
			}
			if json.Unmarshal(data, &f) == nil {
				// Prow's state spelling is not guaranteed case-stable across
				// versions; normalise here so the stored result and the switch
				// in summarize cannot disagree.
				rr.Result = strings.ToUpper(strings.TrimSpace(f.Result))
				if f.Timestamp > 0 {
					rr.Time = time.Unix(f.Timestamp, 0).UTC()
				}
			}
		}
		if rr.Result == "" {
			rr.Result = "MISSING"
		}
		results[i] = rr
		return nil
	})
	if err != nil {
		return nil, err
	}

	return summarize(job, results, window), nil
}

type runPath struct {
	ID      string
	Path    string
	Updated time.Time
}

// indexEntry is one presubmit run as the directory listing reported it, before
// its .txt object is resolved to a run path.
type indexEntry struct {
	id      string
	name    string
	updated time.Time
}

// recentOnly drops run paths that cannot fall inside the window, so a windowed
// lookup does not fetch finished.json for runs it will discard.
func recentOnly(paths []runPath, window time.Duration) []runPath {
	return dropOutsideWindow(paths, func(p runPath) time.Time { return p.Updated }, window)
}

// recentEntries drops presubmit index entries that cannot fall inside the
// window before they are resolved, so a windowed lookup does not spend an
// index GET on every retained run. The listing already carries `updated`, and
// it is the same reference point recentOnly uses.
func recentEntries(entries []indexEntry, window time.Duration) []indexEntry {
	return dropOutsideWindow(entries, func(e indexEntry) time.Time { return e.updated }, window)
}

// dropOutsideWindow keeps only items whose timestamp is inside the window,
// with pruneGrace of slack. The reference point is the newest item rather than
// wall-clock now, so a job that stopped running still summarizes its last
// window. window <= 0, a set with no timestamps and any item without one are
// all left alone: some layouts report no `updated`, and dropping those would
// silently empty the summary.
func dropOutsideWindow[T any](items []T, updated func(T) time.Time, window time.Duration) []T {
	if window <= 0 || len(items) == 0 {
		return items
	}
	var newest time.Time
	for _, it := range items {
		if t := updated(it); t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		return items
	}
	cutoff := newest.Add(-window - pruneGrace)
	out := items[:0]
	for _, it := range items {
		if t := updated(it); !t.IsZero() && t.Before(cutoff) {
			continue
		}
		out = append(out, it)
	}
	return out
}

func enumerate(ctx context.Context, c *gcs.Client, bucket, job string, window time.Duration) ([]runPath, error) {
	// Presubmits are indexed as one tiny .txt per run.
	objs, err := c.List(ctx, bucket, "pr-logs/directory/"+job+"/")
	if err != nil {
		return nil, err
	}
	// Collect the index entries first, then resolve the ones inside the window
	// in parallel: one sequential GET per retained run is the cold-start cost
	// of this command.
	var entries []indexEntry
	for _, o := range objs {
		name := o.Name
		if strings.HasSuffix(name, "/latest-build.txt") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		id := strings.TrimSuffix(baseName(name), ".txt")
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			continue
		}
		updated, _ := time.Parse(time.RFC3339, o.Updated)
		entries = append(entries, indexEntry{id: id, name: name, updated: updated})
	}
	entries = recentEntries(entries, window)

	paths := make([]string, len(entries))
	err = parallel.Do(len(entries), fanOut, func(i int) error {
		data, err := c.Get(ctx, bucket, entries[i].name)
		if err != nil {
			return nil
		}
		p := strings.TrimSpace(string(data))
		p = strings.TrimPrefix(p, "gs://"+bucket+"/")
		p = strings.TrimPrefix(p, "gs://")
		paths[i] = p
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]runPath, 0, len(entries))
	for i, e := range entries {
		if paths[i] == "" {
			continue
		}
		out = append(out, runPath{ID: e.id, Path: paths[i], Updated: e.updated})
	}
	if len(out) > 0 {
		return out, nil
	}

	// Periodics are directories under logs/<job>/.
	dirs, err := c.ListDirs(ctx, bucket, "logs/"+job+"/")
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		d = strings.TrimSuffix(d, "/")
		id := baseName(d)
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			continue
		}
		out = append(out, runPath{ID: id, Path: d})
	}
	// A job that never existed leaves both layouts empty. A job that does exist
	// but whose index entries are unusable still has objects, so it stays an
	// empty summary rather than becoming an error.
	if len(objs) == 0 && len(dirs) == 0 {
		return nil, ErrJobNotFound
	}
	return out, nil
}

func summarize(job string, results []RunResult, window time.Duration) *Summary {
	s := &Summary{Schema: api.SchemaV1, Job: job, Samples: results}
	if len(results) == 0 {
		s.Verdict = VerdictUnknown
		return s
	}
	sortRuns(results)

	to := results[len(results)-1].Time
	cutoff := time.Time{}
	if window > 0 {
		cutoff = to.Add(-window)
	}

	var completed []RunResult
	prs := map[string]struct{}{}
	for _, r := range results {
		if !cutoff.IsZero() && r.Time.Before(cutoff) {
			continue
		}
		// Runs counts the runs the rate is computed over, so a windowed card
		// does not quote a total from outside its own window.
		s.Runs++
		switch r.Result {
		case "SUCCESS":
			s.Pass++
			t := r.Time
			if s.LastPass == nil || t.After(*s.LastPass) {
				s.LastPass = &t
			}
		case "FAILURE", "ERROR":
			s.Fail++
			if pr := prFromPath(r.Path); pr != "" {
				prs[pr] = struct{}{}
			}
		case "ABORTED":
			s.Abort++
			continue
		default:
			s.Missing++
			continue
		}
		completed = append(completed, r)
	}
	s.From = cutoff
	if cutoff.IsZero() && len(results) > 0 {
		s.From = results[0].Time
	}
	s.To = to
	s.DistinctPRs = len(prs)
	if len(prs) > 0 {
		s.FailingPRs = make([]string, 0, len(prs))
		for pr := range prs {
			s.FailingPRs = append(s.FailingPRs, pr)
		}
		// Map iteration is unordered; the card and the JSON must not be.
		sort.Strings(s.FailingPRs)
	}

	if n := s.Pass + s.Fail; n > 0 {
		s.PassRate = float64(s.Pass) / float64(n)
	}

	// Recent rate over the last 10 completed runs.
	recent := completed
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}
	rp := 0
	rf := 0
	for _, r := range recent {
		if r.Result == "SUCCESS" {
			rp++
		} else {
			rf++
		}
	}
	if n := rp + rf; n > 0 {
		s.RecentPassRate = float64(rp) / float64(n)
	}

	for i := len(completed) - 1; i >= 0; i-- {
		if completed[i].Result == "SUCCESS" {
			break
		}
		s.Streak++
	}

	s.Verdict = verdict(s)
	return s
}

func verdict(s *Summary) Verdict {
	completed := s.Pass + s.Fail
	if completed < 5 {
		return VerdictUnknown
	}
	if s.Pass == 0 {
		return VerdictBroken
	}
	if s.Streak >= 20 {
		return VerdictBroken
	}
	if s.LastPass != nil && s.To.Sub(*s.LastPass) >= 14*24*time.Hour {
		return VerdictBroken
	}
	if s.PassRate < 0.85 || (s.PassRate-s.RecentPassRate) >= 0.10 {
		return VerdictFlaky
	}
	if s.PassRate >= 0.95 {
		return VerdictHealthy
	}
	return VerdictDegraded
}

func sortRuns(rs []RunResult) {
	// insertion sort by time; the slice is small and often nearly ordered
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Time.Before(rs[j-1].Time); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// prFromPath extracts "org/repo#N" from pr-logs/pull/<org>_<repo>/<pr>/...
func prFromPath(p string) string {
	const marker = "pr-logs/pull/"
	i := strings.Index(p, marker)
	if i < 0 {
		return ""
	}
	rest := strings.TrimPrefix(p[i:], marker)
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return ""
	}
	// parts[0] is the flattened org_repo. An org can never contain an
	// underscore and a repo can, so the first one is the separator — the same
	// rule prow.Repo uses on a run path.
	org, repo, ok := strings.Cut(parts[0], "_")
	if !ok || org == "" || repo == "" {
		return parts[0] + "#" + parts[1]
	}
	return org + "/" + repo + "#" + parts[1]
}

// FormatRate renders a fraction as a percentage with one decimal.
func FormatRate(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}
