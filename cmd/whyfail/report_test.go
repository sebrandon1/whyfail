package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/health"
)

func TestReadJobList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.txt")
	body := "# curated for the board\n\npull-ci-org-repo-main-unit\n  pull-ci-org-repo-main-e2e  \n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, dups, err := readJobList(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pull-ci-org-repo-main-unit", "pull-ci-org-repo-main-e2e"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("jobs = %v, want %v", got, want)
	}
	if dups != 0 {
		t.Errorf("duplicates = %d, want 0", dups)
	}

	// A repeated name (even with different whitespace) yields one row and is
	// counted, so a hand-editing slip is visible rather than silently doubling
	// a board row.
	dupBody := "pull-ci-org-repo-main-unit\npull-ci-org-repo-main-e2e\n  pull-ci-org-repo-main-unit  \n"
	if err := os.WriteFile(path, []byte(dupBody), 0o644); err != nil {
		t.Fatal(err)
	}
	got, dups, err = readJobList(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("deduped jobs = %v, want %v", got, want)
	}
	if dups != 1 {
		t.Errorf("duplicates = %d, want 1", dups)
	}

	// An empty list is an error: publishing an empty board is worse than failing.
	if err := os.WriteFile(path, []byte("# nothing yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readJobList(path); err == nil {
		t.Error("want an error for a list with no jobs")
	}

	// A missing file is an error, not an empty list.
	if _, _, err := readJobList(filepath.Join(dir, "absent.txt")); err == nil {
		t.Error("want an error for a missing job list")
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
		{Job: "flaky-new", Verdict: "FLAKY", Fail: 2, Regressed: true},
		{Job: "broken", Verdict: "BROKEN", Fail: 1},
		{Job: "unknown", Verdict: "UNKNOWN"},
	}
	sortReportJobs(jobs)

	// A regression outranks the fail count inside a tier, so the job that just
	// broke is above the one that has been flaky all along.
	want := []string{"broken", "flaky-new", "flaky-a", "flaky-b", "degraded", "unknown", "healthy"}
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

func TestLoadHistory(t *testing.T) {
	dir := t.TempDir()

	// A missing file is the first day, not an error.
	if h, err := loadHistory(filepath.Join(dir, "absent.json")); err != nil || h != nil {
		t.Fatalf("missing history = %v, %v; want nil, nil", h, err)
	}

	path := filepath.Join(dir, "history.json")
	body := `{"schema":"whyfail/v1","points":[{"date":"2026-11-22","jobs":{"a":{"verdict":"FLAKY","pass_rate":0.7,"runs":10}}}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := loadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Points) != 1 || h.Points[0].Jobs["a"].Verdict != "FLAKY" {
		t.Fatalf("history = %+v", h)
	}

	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHistory(path); err == nil {
		t.Fatal("want an error for a corrupt history file")
	}
}

func TestPreviousVerdicts(t *testing.T) {
	h := &historyOut{Points: []historyPoint{
		{Date: "2026-11-20", Jobs: map[string]historyJob{"a": {Verdict: "HEALTHY"}, "b": {Verdict: "BROKEN"}}},
		{Date: "2026-11-22", Jobs: map[string]historyJob{"a": {Verdict: "FLAKY"}, "c": {Verdict: "DEGRADED"}}},
		// Same day and a later point must both be ignored.
		{Date: "2026-11-23", Jobs: map[string]historyJob{"a": {Verdict: "BROKEN"}}},
	}}
	got := previousVerdicts(h, "2026-11-23")
	if got["a"] != "FLAKY" || got["c"] != "DEGRADED" {
		t.Errorf("previous = %v, want the 11-22 point", got)
	}
	if _, ok := got["b"]; ok {
		t.Error("a job absent from the newest point must not carry an older verdict")
	}
	if previousVerdicts(nil, "2026-11-23") != nil {
		t.Error("no history must yield no previous verdicts")
	}
	only := &historyOut{Points: []historyPoint{{Date: "2026-11-23", Jobs: map[string]historyJob{"a": {Verdict: "BROKEN"}}}}}
	if got := previousVerdicts(only, "2026-11-23"); got != nil {
		t.Errorf("a same-day-only history must yield nothing, got %v", got)
	}
}

func TestMarkRegressions(t *testing.T) {
	cases := []struct {
		name    string
		today   health.Verdict
		prev    health.Verdict
		present bool
		regress bool
		prevOut health.Verdict
	}{
		{"healthy to flaky", "FLAKY", "HEALTHY", true, true, "HEALTHY"},
		{"flaky to broken", "BROKEN", "FLAKY", true, true, "FLAKY"},
		{"flaky to healthy", "HEALTHY", "FLAKY", true, false, "FLAKY"},
		{"unchanged", "FLAKY", "FLAKY", true, false, "FLAKY"},
		{"unknown before", "BROKEN", "UNKNOWN", true, false, ""},
		{"unknown now", "UNKNOWN", "HEALTHY", true, false, ""},
		{"no previous point", "BROKEN", "", false, false, ""},
	}
	for _, c := range cases {
		jobs := []reportJob{{Job: "j", Verdict: c.today}}
		prev := map[string]health.Verdict{}
		if c.present {
			prev["j"] = c.prev
		}
		markRegressions(jobs, prev)
		if jobs[0].Regressed != c.regress {
			t.Errorf("%s: regressed = %v, want %v", c.name, jobs[0].Regressed, c.regress)
		}
		if jobs[0].PreviousVerdict != c.prevOut {
			t.Errorf("%s: previous = %q, want %q", c.name, jobs[0].PreviousVerdict, c.prevOut)
		}
	}

	// A nil map must be safe.
	jobs := []reportJob{{Job: "j", Verdict: "BROKEN"}}
	markRegressions(jobs, nil)
	if jobs[0].Regressed {
		t.Error("nil previous map must not mark anything")
	}
}
