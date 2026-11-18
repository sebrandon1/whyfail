package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadJobList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.txt")
	body := "# curated for the board\n\npull-ci-org-repo-main-unit\n  pull-ci-org-repo-main-e2e  \n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readJobList(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pull-ci-org-repo-main-unit", "pull-ci-org-repo-main-e2e"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("jobs = %v, want %v", got, want)
	}

	// An empty list is an error: publishing an empty board is worse than failing.
	if err := os.WriteFile(path, []byte("# nothing yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readJobList(path); err == nil {
		t.Error("want an error for a list with no jobs")
	}
}

// The board is read top-down, so the worst rows must come first and the order
// must not wobble between snapshots.
func TestSortReportJobs(t *testing.T) {
	jobs := []reportJob{
		{Job: "healthy", Verdict: "HEALTHY", Fail: 0},
		{Job: "degraded", Verdict: "DEGRADED", Fail: 3},
		{Job: "flaky-b", Verdict: "FLAKY", Fail: 5},
		{Job: "flaky-a", Verdict: "FLAKY", Fail: 9},
		{Job: "broken", Verdict: "BROKEN", Fail: 1},
		{Job: "unknown", Verdict: "UNKNOWN"},
	}
	sortReportJobs(jobs)

	want := []string{"broken", "flaky-a", "flaky-b", "degraded", "unknown", "healthy"}
	for i, w := range want {
		if jobs[i].Job != w {
			t.Errorf("row %d = %q, want %q (order: %v)", i, jobs[i].Job, w, jobNames(jobs))
		}
	}
}

func jobNames(jobs []reportJob) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.Job
	}
	return out
}

// A same-day re-run replaces that day's point instead of adding a second one,
// and old points are pruned so the file cannot grow without bound.
func TestAppendHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	day := time.Date(2026, 11, 18, 5, 17, 0, 0, time.UTC)
	jobs := []reportJob{
		{Job: "a", Verdict: "BROKEN", PassRate: 0, Runs: 10},
		{Job: "b", Verdict: "HEALTHY", PassRate: 1, Runs: 10},
	}

	if err := appendHistory(path, day, jobs, 3); err != nil {
		t.Fatal(err)
	}
	// A second run on the same day, with a changed verdict for one job.
	jobs[0].Verdict = "FLAKY"
	if err := appendHistory(path, day.Add(2*time.Hour), jobs, 3); err != nil {
		t.Fatal(err)
	}

	h := readHistory(t, path)
	if len(h.Points) != 1 {
		t.Fatalf("points = %d, want 1 after a same-day re-run", len(h.Points))
	}
	if got := h.Points[0].Jobs["a"].Verdict; got != "FLAKY" {
		t.Errorf("verdict = %q, want the newer FLAKY", got)
	}

	// Four more days, keeping three: the oldest points fall off.
	for i := 1; i <= 4; i++ {
		if err := appendHistory(path, day.AddDate(0, 0, i), jobs, 3); err != nil {
			t.Fatal(err)
		}
	}
	h = readHistory(t, path)
	if len(h.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(h.Points))
	}
	if h.Points[len(h.Points)-1].Date != "2026-11-22" {
		t.Errorf("last point = %q, want the newest day", h.Points[len(h.Points)-1].Date)
	}
	if h.Points[0].Date != "2026-11-20" {
		t.Errorf("first point = %q, want the retained window's start", h.Points[0].Date)
	}
}

func readHistory(t *testing.T, path string) historyOut {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var h historyOut
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

// A corrupt history file is an error rather than a silent reset: losing the
// trend without saying so would be the worst of both.
func TestAppendHistoryRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendHistory(path, time.Now(), []reportJob{{Job: "a"}}, 10); err == nil {
		t.Fatal("want an error for a corrupt history file")
	}
}

// The snapshot says which window it summarised, in the units a reader uses.
func TestWindowLabel(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "all retained"},
		{-time.Hour, "all retained"},
		{720 * time.Hour, "720h"},
		{24 * time.Hour, "24h"},
		{90 * time.Minute, "1h30m0s"},
	}
	for _, c := range cases {
		if got := windowLabel(c.d); got != c.want {
			t.Errorf("windowLabel(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
