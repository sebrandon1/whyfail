// Package health computes job-level pass rates from Prow's retained run index.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebrandon1/whyfail/internal/gcs"
)

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
	Job            string      `json:"job"`
	From           time.Time   `json:"from"`
	To             time.Time   `json:"to"`
	Runs           int         `json:"runs"`
	Pass           int         `json:"pass"`
	Fail           int         `json:"fail"`
	Abort          int         `json:"abort"`
	Missing        int         `json:"missing"`
	PassRate       float64     `json:"pass_rate"`
	RecentPassRate float64     `json:"recent_pass_rate"`
	Streak         int         `json:"streak"`
	LastPass       *time.Time  `json:"last_pass_at,omitempty"`
	DistinctPRs    int         `json:"distinct_prs_failing"`
	Verdict        Verdict     `json:"verdict"`
	Samples        []RunResult `json:"-"`
}

// Collect enumerates a job's runs and summarizes them. window <= 0 means the
// full retained history.
func Collect(ctx context.Context, c *gcs.Client, bucket, job string, window time.Duration) (*Summary, error) {
	paths, err := enumerate(ctx, c, bucket, job)
	if err != nil {
		return nil, err
	}

	results := make([]RunResult, 0, len(paths))
	var mu sync.Mutex
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup

	for _, p := range paths {
		wg.Add(1)
		go func(p runPath) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			rr := RunResult{ID: p.ID, Path: p.Path, Time: p.Updated}
			data, err := c.Get(ctx, bucket, p.Path+"/finished.json")
			if err == nil {
				var f struct {
					Result    string `json:"result"`
					Timestamp int64  `json:"timestamp"`
				}
				if json.Unmarshal(data, &f) == nil {
					rr.Result = f.Result
					if f.Timestamp > 0 {
						rr.Time = time.Unix(f.Timestamp, 0).UTC()
					}
				}
			}
			if rr.Result == "" {
				rr.Result = "MISSING"
			}
			mu.Lock()
			results = append(results, rr)
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	return summarize(job, results, window), nil
}

type runPath struct {
	ID      string
	Path    string
	Updated time.Time
}

func enumerate(ctx context.Context, c *gcs.Client, bucket, job string) ([]runPath, error) {
	// Presubmits are indexed as one tiny .txt per run.
	objs, err := c.List(ctx, bucket, "pr-logs/directory/"+job+"/")
	if err != nil {
		return nil, err
	}
	var out []runPath
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

		data, err := c.Get(ctx, bucket, name)
		if err != nil {
			continue
		}
		path := strings.TrimSpace(string(data))
		path = strings.TrimPrefix(path, "gs://"+bucket+"/")
		path = strings.TrimPrefix(path, "gs://")
		if path == "" {
			continue
		}
		out = append(out, runPath{ID: id, Path: path, Updated: updated})
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
	return out, nil
}

func summarize(job string, results []RunResult, window time.Duration) *Summary {
	s := &Summary{Job: job, Samples: results}
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
		s.Runs++
		if !cutoff.IsZero() && r.Time.Before(cutoff) {
			continue
		}
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
	return parts[0] + "#" + parts[1]
}

// FormatRate renders a fraction as a percentage with one decimal.
func FormatRate(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}
