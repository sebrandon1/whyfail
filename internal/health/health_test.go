package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/gcs"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeClient(t *testing.T, objects map[string]string, prefixes map[string][]string) *gcs.Client {
	t.Helper()
	return fakeClientAt(t, objects, prefixes, nil)
}

// fakeClientAt is fakeClient with an `updated` timestamp per listing entry.
func fakeClientAt(t *testing.T, objects map[string]string, prefixes map[string][]string, updated func(string) time.Time) *gcs.Client {
	t.Helper()
	c := gcs.New(t.TempDir())
	c.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/storage/v1/") {
			prefix := r.URL.Query().Get("prefix")
			if r.URL.Query().Get("delimiter") == "/" {
				body, _ := json.Marshal(map[string]any{"prefixes": prefixes[prefix]})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
			}
			var names []string
			for n := range objects {
				if strings.HasPrefix(n, prefix) {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			items := make([]map[string]string, 0, len(names))
			for _, n := range names {
				item := map[string]string{"name": n, "size": "1"}
				if updated != nil {
					if ts := updated(n); !ts.IsZero() {
						item["updated"] = ts.Format(time.RFC3339)
					}
				}
				items = append(items, item)
			}
			body, _ := json.Marshal(map[string]any{"items": items})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
		}
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), "bkt/")
		if body, ok := objects[key]; ok {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})}
	return c
}

func TestCollectPresubmitIndex(t *testing.T) {
	objects := map[string]string{
		"pr-logs/directory/job/100.txt":            "gs://bkt/pr-logs/pull/o_r/1/job/100",
		"pr-logs/directory/job/101.txt":            "gs://bkt/pr-logs/pull/o_r/1/job/101",
		"pr-logs/directory/job/102.txt":            "gs://bkt/pr-logs/pull/o_r/2/job/102",
		"pr-logs/directory/job/latest-build.txt":   "102",
		"pr-logs/directory/job/notes.txt":          "gs://bkt/ignored",
		"pr-logs/pull/o_r/1/job/100/finished.json": `{"result":"SUCCESS"}`,
		"pr-logs/pull/o_r/1/job/101/finished.json": `{"result":"FAILURE"}`,
		"pr-logs/pull/o_r/2/job/102/finished.json": `{"result":"ABORTED"}`,
	}
	c := fakeClient(t, objects, nil)

	s, err := Collect(context.Background(), c, "bkt", "job", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Runs != 3 || s.Pass != 1 || s.Fail != 1 || s.Abort != 1 {
		t.Fatalf("summary: %+v", s)
	}
	if s.DistinctPRs != 1 {
		t.Fatalf("distinct prs: %d", s.DistinctPRs)
	}
}

func TestCollectPeriodicDirs(t *testing.T) {
	objects := map[string]string{
		"logs/job/200/finished.json": `{"result":"FAILURE"}`,
		"logs/job/201/finished.json": `{"result":"SUCCESS"}`,
	}
	prefixes := map[string][]string{
		"logs/job/": {"logs/job/200/", "logs/job/201/", "logs/job/current/"},
	}
	c := fakeClient(t, objects, prefixes)

	s, err := Collect(context.Background(), c, "bkt", "job", 0)
	if err != nil {
		t.Fatal(err)
	}
	// "current" is not numeric and must be skipped.
	if s.Runs != 2 || s.Pass != 1 || s.Fail != 1 {
		t.Fatalf("summary: %+v", s)
	}
}

func TestCollectMissingFinishedIsNotFailure(t *testing.T) {
	objects := map[string]string{
		"logs/job/300/finished.json": `{"result":"SUCCESS"}`,
	}
	prefixes := map[string][]string{"logs/job/": {"logs/job/300/", "logs/job/301/"}}
	c := fakeClient(t, objects, prefixes)

	s, err := Collect(context.Background(), c, "bkt", "job", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Missing != 1 || s.Fail != 0 {
		t.Fatalf("pruned run must be missing, not failure: %+v", s)
	}
}

func build(results ...string) []RunResult {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]RunResult, 0, len(results))
	for i, r := range results {
		out = append(out, RunResult{
			ID:     "1",
			Path:   "pr-logs/pull/org_repo/7/job/1",
			Result: r,
			Time:   base.Add(time.Duration(i) * time.Hour),
		})
	}
	return out
}

func repeat(n int, r string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = r
	}
	return out
}

func TestVerdicts(t *testing.T) {
	broke := summarize("j", build(repeat(10, "FAILURE")...), 0)
	if broke.Verdict != "BROKEN" {
		t.Fatalf("all failures: got %s", broke.Verdict)
	}

	flaky := summarize("j", build(append(repeat(6, "SUCCESS"), repeat(4, "FAILURE")...)...), 0)
	if flaky.Verdict != "FLAKY" {
		t.Fatalf("60%%: got %s (rate %.2f)", flaky.Verdict, flaky.PassRate)
	}

	healthy := summarize("j", build(append(repeat(20, "SUCCESS"), repeat(1, "FAILURE")...)...), 0)
	if healthy.Verdict != "HEALTHY" {
		t.Fatalf("95%%: got %s (rate %.2f)", healthy.Verdict, healthy.PassRate)
	}

	unknown := summarize("j", build("SUCCESS", "FAILURE"), 0)
	if unknown.Verdict != "UNKNOWN" {
		t.Fatalf("few runs: got %s", unknown.Verdict)
	}

	mixed := summarize("j", build(append(repeat(9, "SUCCESS"), repeat(1, "FAILURE")...)...), 0)
	if mixed.Verdict != "DEGRADED" {
		t.Fatalf("90%%: got %s", mixed.Verdict)
	}
}

func TestAbortsExcludedAndStreak(t *testing.T) {
	rs := build("SUCCESS", "FAILURE", "ABORTED", "ABORTED", "FAILURE")
	s := summarize("j", rs, 0)
	if s.Abort != 2 || s.Pass != 1 || s.Fail != 2 {
		t.Fatalf("counts: %+v", s)
	}
	if s.Streak != 2 {
		t.Fatalf("streak: %d", s.Streak)
	}
	if s.DistinctPRs != 1 {
		t.Fatalf("prs: %d", s.DistinctPRs)
	}
}

func TestPRFromPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"pr-logs/pull/rh-ecosystem-edge_recert/2364/job/1", "rh-ecosystem-edge/recert#2364"},
		// The org has no underscore and a repo may, so the first one separates.
		{"pr-logs/pull/org_my_repo/7/job/9", "org/my_repo#7"},
		// A segment with nothing to split still names the PR rather than
		// dropping it.
		{"pr-logs/pull/nounderscore/7/job/9", "nounderscore#7"},
		{"logs/periodic/123", ""},
		{"pr-logs/pull/onlyone", ""},
	}
	for _, c := range cases {
		if got := prFromPath(c.in); got != c.want {
			t.Errorf("prFromPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVerdictBoundaries(t *testing.T) {
	// A long losing streak is broken even though the overall rate is fine.
	streak := summarize("j", build(append(repeat(10, "SUCCESS"), repeat(20, "FAILURE")...)...), 0)
	if streak.Verdict != VerdictBroken || streak.Streak != 20 {
		t.Fatalf("streak 20: got %s streak=%d", streak.Verdict, streak.Streak)
	}

	// No pass in the last 14 days is broken regardless of history.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var stale []RunResult
	for i := 0; i < 5; i++ {
		stale = append(stale, RunResult{Result: "SUCCESS", Time: base.Add(time.Duration(i) * time.Hour)})
	}
	for i := 0; i < 5; i++ {
		stale = append(stale, RunResult{Result: "FAILURE", Time: base.Add(20*24*time.Hour + time.Duration(i)*time.Hour)})
	}
	if s := summarize("j", stale, 0); s.Verdict != VerdictBroken {
		t.Fatalf("stale last pass: got %s", s.Verdict)
	}
}

func TestIsBad(t *testing.T) {
	for _, v := range []Verdict{VerdictBroken, VerdictFlaky} {
		if !v.IsBad() {
			t.Errorf("%s should be bad", v)
		}
	}
	for _, v := range []Verdict{VerdictDegraded, VerdictHealthy, VerdictUnknown} {
		if v.IsBad() {
			t.Errorf("%s should not be bad", v)
		}
	}
}

func TestWindowAndMissing(t *testing.T) {
	// Five completed runs one hour apart; a 2h window keeps only the last three.
	rs := build("SUCCESS", "FAILURE", "SUCCESS", "FAILURE", "SUCCESS")
	rs = append(rs, RunResult{Result: "GONE", Time: rs[len(rs)-1].Time.Add(time.Hour)})
	s := summarize("j", rs, 2*time.Hour)

	// Runs describes the same set as the rate: the three inside the window, one
	// of which is missing a finished.json.
	if s.Runs != 3 {
		t.Fatalf("runs: %d", s.Runs)
	}
	if s.Missing != 1 {
		t.Fatalf("missing: %d", s.Missing)
	}
	// cutoff = last run - 2h, so runs at t=3h,4h,5h count: FAILURE, SUCCESS (and the missing one).
	if s.Pass+s.Fail != 2 {
		t.Fatalf("in-window completed: pass=%d fail=%d", s.Pass, s.Fail)
	}
}

func TestRecentOnly(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	paths := []runPath{
		{ID: "1", Updated: at(0)},
		{ID: "2", Updated: at(170)},
		{ID: "3", Updated: at(200)},
	}

	// A 24h window with a 12h grace keeps the newest run and the one inside the
	// grace band, and drops the run from a week earlier.
	got := recentOnly(paths, 24*time.Hour)
	if len(got) != 2 || got[0].ID != "2" || got[1].ID != "3" {
		t.Fatalf("pruned to %+v", got)
	}

	// No window and an all-zero layout both leave the list untouched.
	if n := len(recentOnly(paths, 0)); n != 3 {
		t.Fatalf("window 0 pruned to %d", n)
	}
	unstyled := []runPath{{ID: "1"}, {ID: "2"}}
	if n := len(recentOnly(unstyled, time.Hour)); n != 2 {
		t.Fatalf("zero timestamps pruned to %d", n)
	}

	// An entry with no timestamp survives next to timestamped ones.
	mixed := append([]runPath{{ID: "old", Updated: at(0)}}, runPath{ID: "periodic"}, runPath{ID: "new", Updated: at(200)})
	kept := recentOnly(mixed, time.Hour)
	if len(kept) != 2 || kept[0].ID != "periodic" || kept[1].ID != "new" {
		t.Fatalf("mixed = %+v", kept)
	}
}

func TestCollectWindowPrunesFetch(t *testing.T) {
	// Thirty index entries a day apart; a 48h window must not fetch the other
	// finished.json objects.
	objects := map[string]string{}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		id := strconv.Itoa(1000 + i)
		run := "gs://bkt/pr-logs/pull/o_r/1/job/" + id
		objects["pr-logs/directory/job/"+id+".txt"] = run
		objects["pr-logs/pull/o_r/1/job/"+id+"/finished.json"] = `{"result":"SUCCESS"}`
	}
	c := fakeClientAt(t, objects, nil, func(name string) time.Time {
		for i := 0; i < 30; i++ {
			if id := strconv.Itoa(1000 + i); strings.HasSuffix(name, "/"+id+".txt") {
				return base.Add(time.Duration(i) * 24 * time.Hour)
			}
		}
		return time.Time{}
	})

	s, err := Collect(context.Background(), c, "bkt", "job", 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// The last run plus the two inside the grace band.
	if s.Runs != 3 {
		t.Fatalf("runs = %d, want 3", s.Runs)
	}
	if s.Pass != 3 {
		t.Fatalf("pass = %d", s.Pass)
	}
}

func TestFormatRateAndBaseName(t *testing.T) {
	if got := FormatRate(0.809); got != "80.9%" {
		t.Errorf("got %q", got)
	}
	if got := FormatRate(0); got != "0.0%" {
		t.Errorf("got %q", got)
	}
	if got := baseName("pr-logs/pull/org_repo/1/job/2123"); got != "2123" {
		t.Errorf("got %q", got)
	}
	if got := baseName("single"); got != "single" {
		t.Errorf("got %q", got)
	}
}

func TestCollectUnknownJob(t *testing.T) {
	c := fakeClient(t, map[string]string{}, nil)
	_, err := Collect(context.Background(), c, "bkt", "pull-ci-org-repo-main-nope", 0)
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

func TestCollectPeriodicUnknownJob(t *testing.T) {
	c := fakeClient(t, map[string]string{}, map[string][]string{})
	if _, err := Collect(context.Background(), c, "bkt", "periodic-nope", 0); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

func TestCollectUnusableIndexIsNotAnError(t *testing.T) {
	// The job exists but every index entry is unusable, which is a real job with
	// an odd index rather than a name that never existed.
	objects := map[string]string{
		"pr-logs/directory/job/latest-build.txt": "102",
		"pr-logs/directory/job/notes.txt":        "gs://bkt/pr-logs/pull/o_r/1/job/102",
	}
	c := fakeClient(t, objects, nil)

	s, err := Collect(context.Background(), c, "bkt", "job", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Runs != 0 || s.Verdict != VerdictUnknown {
		t.Fatalf("got runs=%d verdict=%s", s.Runs, s.Verdict)
	}
}

func TestCollectKeepsRunsOrderedByTime(t *testing.T) {
	// Listing order (by object name) is the reverse of timestamp order; the
	// summary must still come back sorted after the parallel fetch.
	objects := map[string]string{
		"pr-logs/directory/job/500.txt":            "gs://bkt/pr-logs/pull/o_r/1/job/500",
		"pr-logs/directory/job/501.txt":            "gs://bkt/pr-logs/pull/o_r/1/job/501",
		"pr-logs/pull/o_r/1/job/500/finished.json": `{"result":"SUCCESS","timestamp":3000}`,
		"pr-logs/pull/o_r/1/job/501/finished.json": `{"result":"FAILURE","timestamp":1000}`,
	}
	c := fakeClient(t, objects, nil)

	s, err := Collect(context.Background(), c, "bkt", "job", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Runs != 2 || s.Pass != 1 || s.Fail != 1 {
		t.Fatalf("runs=%d pass=%d fail=%d", s.Runs, s.Pass, s.Fail)
	}
	if len(s.Samples) != 2 {
		t.Fatalf("samples: %d", len(s.Samples))
	}
	if !s.Samples[0].Time.Before(s.Samples[1].Time) {
		t.Fatalf("samples not sorted: %v then %v", s.Samples[0].Time, s.Samples[1].Time)
	}
}

// The PRs behind the count are named, sorted, and absent when nothing failed.
func TestSummarizeFailingPRs(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	results := []RunResult{
		{Path: "pr-logs/pull/org_repo/9/job/1", Result: "FAILURE", Time: base},
		{Path: "pr-logs/pull/org_repo/7/job/2", Result: "FAILURE", Time: base.Add(time.Hour)},
		{Path: "pr-logs/pull/org_repo/9/job/3", Result: "FAILURE", Time: base.Add(2 * time.Hour)},
		{Path: "pr-logs/pull/org_repo/7/job/4", Result: "SUCCESS", Time: base.Add(3 * time.Hour)},
	}
	s := summarize("j", results, 0)
	if s.DistinctPRs != 2 {
		t.Fatalf("distinct = %d, want 2", s.DistinctPRs)
	}
	want := []string{"org/repo#7", "org/repo#9"}
	if len(s.FailingPRs) != len(want) || s.FailingPRs[0] != want[0] || s.FailingPRs[1] != want[1] {
		t.Errorf("failing prs = %v, want %v", s.FailingPRs, want)
	}

	// A passing job names none, so the field stays out of the JSON.
	healthy := summarize("j", build(repeat(10, "SUCCESS")...), 0)
	if healthy.FailingPRs != nil {
		t.Errorf("healthy job listed %v", healthy.FailingPRs)
	}
}

// The repository comes from the run paths, and only when they are PR paths: a
// periodic job has no repository, and an underscore is what separates org from
// repo.
func TestSummaryRepo(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  string
	}{
		{"pr run", []string{"pr-logs/pull/rh-ecosystem-edge_recert/2364/job/9"}, "rh-ecosystem-edge/recert"},
		{"mixed", []string{"logs/periodic-x/1", "pr-logs/pull/org_repo/2/job/3"}, "org/repo"},
		{"periodic only", []string{"logs/pull-ci-org-repo-main-unit/123"}, ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		s := &Summary{}
		for _, p := range c.paths {
			s.Samples = append(s.Samples, RunResult{Path: p})
		}
		if got := s.Repo(); got != c.want {
			t.Errorf("%s: Repo = %q, want %q", c.name, got, c.want)
		}
	}
}
