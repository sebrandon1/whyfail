package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/gcs"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeClient(t *testing.T, objects map[string]string, prefixes map[string][]string) *gcs.Client {
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
				items = append(items, map[string]string{"name": n, "size": "1"})
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
	if got := prFromPath("pr-logs/pull/rh-ecosystem-edge_recert/2364/job/1"); got != "rh-ecosystem-edge_recert#2364" {
		t.Fatalf("got %q", got)
	}
	if got := prFromPath("logs/periodic/123"); got != "" {
		t.Fatalf("got %q", got)
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

	if s.Runs != 6 {
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
