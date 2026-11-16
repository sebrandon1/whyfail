package render

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/run"
	"github.com/sebrandon1/whyfail/internal/stats"
	"github.com/sebrandon1/whyfail/internal/term"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./internal/render -update
var update = flag.Bool("update", false, "rewrite golden files")

// testStyle is plain text at a fixed width, so goldens are stable everywhere.
func testStyle() term.Style {
	return term.Config(new(bytes.Buffer), "never", 100)
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test ./internal/render -update)", name, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0s"},
		{-5, "0s"},
		{45, "45s"},
		{150, "2m30s"},
		{10942, "3h02m"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.in); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{3 << 30, "3.0 GB"},
		{1 << 40, "1.0 TB"},
	}
	for _, c := range cases {
		if got := FormatBytes(c.in); got != c.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{42, "42"},
		{1500, "1.5K"},
		{2_500_000, "2.5M"},
		{1_200_000_000, "1.2B"},
	}
	for _, c := range cases {
		if got := FormatCount(c.in); got != c.want {
			t.Errorf("FormatCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"hello world", 5, "hell…"},
		{"héllo wörld", 6, "héllo…"},
		{"anything", 0, ""},
		{"abc", 1, "…"},
	}
	for _, c := range cases {
		if got := term.Trunc(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestVerdictLabel(t *testing.T) {
	cases := []struct {
		in    health.Verdict
		icon  string
		label string
	}{
		{health.VerdictBroken, "⛔", "BROKEN"},
		{health.VerdictFlaky, "⚠", "FLAKY"},
		{health.VerdictDegraded, "~", "DEGRADED"},
		{health.VerdictHealthy, "✓", "HEALTHY"},
		{health.Verdict(""), "?", "UNKNOWN"},
	}
	for _, c := range cases {
		icon, label := VerdictLabel(c.in)
		if icon != c.icon || label != c.label {
			t.Errorf("VerdictLabel(%q) = %q,%q want %q,%q", c.in, icon, label, c.icon, c.label)
		}
	}
}

func TestDownloadSeconds(t *testing.T) {
	cases := []struct {
		name string
		a    stats.Aggregate
		want float64
	}{
		{"no wall time", stats.Aggregate{Totals: stats.Totals{ReadBytes: 100, NetworkBytes: 100}}, 0},
		{"no network bytes", stats.Aggregate{Totals: stats.Totals{ReadBytes: 100, WallMS: 10}}, 0},
		{"all from network", stats.Aggregate{Totals: stats.Totals{ReadBytes: 100, NetworkBytes: 100, WallMS: 10}}, 0},
		{"some cached", stats.Aggregate{Totals: stats.Totals{ReadBytes: 3000, NetworkBytes: 1000, WallMS: 1000}}, 2},
	}
	for _, c := range cases {
		if got := downloadSeconds(c.a); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func fullDigest() *run.Digest {
	now := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)
	last := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return &run.Digest{
		Schema: "whyfail/v1",
		Job: &run.JobInfo{Name: "pull-ci-org-repo-main-e2e", Health: &health.Summary{
			Job: "pull-ci-org-repo-main-e2e", Pass: 19, Fail: 122, Abort: 34, Runs: 175,
			PassRate: 0.135, From: now.AddDate(0, 0, -60), LastPass: &last, Verdict: health.VerdictFlaky,
		}},
		Run: &run.RunInfo{
			ID: "2123", JobName: "pull-ci-org-repo-main-e2e", Result: "failure",
			URL: "https://prow.ci.openshift.org/view/gs/b/p", Reason: "step failed",
			Cluster: "build01", Namespace: "ci-op-abcdefgh",
			PR:     &run.PRInfo{Org: "org", Repo: "repo", Number: 2364, Author: "alice", Title: "fix"},
			Timing: run.Timing{DurationS: 300, TimeoutS: 600, PctOfTimeout: 50},
			Steps: run.StepsInfo{
				Total:   19,
				Failed:  []run.StepRef{{Name: "e2e-flow", DurationS: 60, Error: "boom"}},
				Longest: []run.StepRef{{Name: "install", DurationS: 120}},
			},
		},
		Tests: &run.Tests{
			Counts:       run.TestCounts{Passed: 247, Failed: 3, Skipped: 497, Flaked: 1},
			Source:       "extension",
			FailedTotal:  3,
			MonitorTotal: 4,
			Failed: []run.TestFailure{
				{Name: "[Late] Prometheus targets", Blocking: true, Lifecycle: "blocking", Error: "targets down"},
				{Name: "informing test", Lifecycle: "informing"},
			},
			Monitors: []run.TestFailure{
				{Name: "mon", Signature: "dns lookup failed: thanos"},
			},
			Causes: []run.CauseGroup{{Signature: "dns lookup failed: thanos", Count: 4}},
		},
		Infra: &run.Infra{
			ClusterProfile: "ipi/aws", Region: "us-east-1", Variant: "amd64",
			Lease:         &run.LeaseInfo{Slice: "240", AvailableAtAcquire: 0, AcquireS: 1.5},
			MachinesTotal: 6,
			Execution:     &run.ExecInfo{Success: false, ErrorCount: 1, DurationS: 300},
			InstallFailed: true,
		},
		Artifacts: &run.Artifacts{
			TotalBytes: 12345678, Count: 42,
			Notable: []run.Notable{
				{Short: "prowjob.json", Bytes: 2048, Why: "run identity and PR linkage"},
				{Short: "build-log.txt", Bytes: 64000, Why: "orchestration timeline"},
				{Short: "artifacts/e2e-flow/build-log.txt", Bytes: 60743, Why: "failing step log"},
				{Short: "artifacts/e2e-flow/artifacts/junit/junit_e2e.xml", Bytes: 4096, Why: "JUnit e2e results"},
			},
		},
		Signals: &run.Signals{
			Sippy:    "not tracked",
			SearchCI: &run.SearchSig{Matches48h: 7, KnownBug: "OCPBUGS-1"},
		},
		NextAction: "retest",
	}
}

func TestDigestGolden(t *testing.T) {
	var buf bytes.Buffer
	Digest(&buf, fullDigest(), testStyle())
	golden(t, "digest-full.txt", buf.String())
}

func TestDigestMinimalRun(t *testing.T) {
	d := &run.Digest{
		Run: &run.RunInfo{ID: "1", JobName: "periodic-x", Result: "success",
			URL: "https://prow.ci.openshift.org/view/gs/b/p"},
	}
	var buf bytes.Buffer
	Digest(&buf, d, testStyle())
	out := buf.String()
	golden(t, "digest-minimal.txt", out)
	if !strings.HasPrefix(out, "periodic-x\n") {
		t.Errorf("expected bare job name header, got %q", out)
	}
}

func TestDigestFallsBackToMonitorGroups(t *testing.T) {
	d := &run.Digest{
		Run: &run.RunInfo{ID: "1", JobName: "j", URL: "u"},
		Tests: &run.Tests{Monitors: []run.TestFailure{
			{Name: "m1", Signature: "same"}, {Name: "m2", Signature: "same"},
		}},
	}
	var buf bytes.Buffer
	Digest(&buf, d, testStyle())
	out := buf.String()
	golden(t, "digest-monitors.txt", out)
	if !strings.Contains(out, "2 failures · 1 cause") {
		t.Errorf("got %q", out)
	}
}

func TestDigestNarrowWidth(t *testing.T) {
	var buf bytes.Buffer
	Digest(&buf, fullDigest(), term.Config(&buf, "never", 60))
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "  URL") {
			continue // the URL is printed whole so it stays copyable
		}
		if n := term.Visible(line); n > 60 {
			t.Errorf("line is %d columns wide at width 60: %q", n, line)
		}
	}
}

func TestFailuresGolden(t *testing.T) {
	tests := &run.Tests{
		Counts: run.TestCounts{Passed: 10},
		Failed: []run.TestFailure{
			{Name: "[sig-x] thing", Blocking: true, Error: "boom\nsecond line"},
			{Name: "informant", Lifecycle: "informing"},
		},
		Monitors: []run.TestFailure{{Name: "monitor-a"}},
	}
	var buf bytes.Buffer
	Failures(&buf, testStyle(), "pull-ci-org-repo-main-e2e", "failure",
		[]run.StepRef{{Name: "e2e-flow", DurationS: 60}}, tests)
	golden(t, "failures.txt", buf.String())
}

func TestJobCard(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		s    health.Summary
		want string
	}{
		{
			name: "no runs",
			s:    health.Summary{Job: "j", Verdict: health.VerdictUnknown},
			want: "no runs found",
		},
		{
			name: "broken",
			s: health.Summary{Job: "j", Runs: 10, Fail: 10, PassRate: 0, RecentPassRate: 0,
				Streak: 10, From: from, To: to, Verdict: health.VerdictBroken},
			want: "retesting will not help",
		},
		{
			name: "flaky",
			s: health.Summary{Job: "j", Runs: 10, Pass: 6, Fail: 4, PassRate: 0.6,
				RecentPassRate: 0.5, From: from, To: to, LastPass: &last, Verdict: health.VerdictFlaky},
			want: "a retest is reasonable",
		},
		{
			name: "healthy",
			s: health.Summary{Job: "j", Runs: 10, Pass: 10, PassRate: 1, RecentPassRate: 1,
				From: from, To: to, LastPass: &to, Verdict: health.VerdictHealthy},
			want: "passes reliably",
		},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		Job(&buf, &c.s, testStyle())
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("%s: missing %q in %q", c.name, c.want, buf.String())
		}
	}
}

func TestJobGolden(t *testing.T) {
	from := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	Job(&buf, &health.Summary{
		Job: "pull-ci-org-repo-main-e2e", Runs: 154, Pass: 98, Fail: 56,
		PassRate: 0.638, RecentPassRate: 0.61, Streak: 2, LastPass: &last,
		DistinctPRs: 9, From: from, To: to, Verdict: health.VerdictFlaky,
	}, testStyle())
	golden(t, "job-flaky.txt", buf.String())
}

func TestGainEmptyAndPopulated(t *testing.T) {
	var buf bytes.Buffer
	Gain(&buf, stats.Aggregate{}, 4, 0, testStyle())
	if !strings.Contains(buf.String(), "no recorded calls yet") {
		t.Fatalf("empty: %q", buf.String())
	}

	a := stats.Aggregate{
		Since: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Totals: stats.Totals{
			Calls: 2, ReadBytes: 1000, EmittedBytes: 100,
			NetworkBytes: 300, InventoryBytes: 5000, CacheHits: 1, WallMS: 1000,
		},
		Groups:   map[string]stats.Totals{"run": {Calls: 2, ReadBytes: 1000}},
		Top:      []stats.Record{{Cmd: "run", Target: "job/1", ReadBytes: 1000, EmittedBytes: 100}},
		Verdicts: map[string]int{"FLAKY": 2},
	}
	buf.Reset()
	Gain(&buf, a, 4, 10*time.Minute, testStyle())
	out := buf.String()
	for _, want := range []string{
		"saved", "2 calls", "since 2026-10-14", "10.0% of input",
		"~tokens saved", "run", "top saves", "job/1",
		"(900 B ÷ 4 bytes/token)",
		"manual time saved", "from cache (70%)", "2 FLAKY", "█",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	golden(t, "gain.txt", out)
}

func TestPRCardLine(t *testing.T) {
	st := term.Config(new(bytes.Buffer), "never", 100)
	const job = "pull-ci-org-repo-main-e2e-aws"

	// A job that is not in the bucket must not panic or read as healthy.
	unknown := PRCardLine(st, PRCard{Job: job, Prow: true}, 40)
	if !strings.Contains(unknown, "UNKNOWN") || !strings.Contains(unknown, "no history in this bucket") {
		t.Errorf("nil health: %q", unknown)
	}

	// A check with no Prow job behind it is a failure, not an unknown job.
	nonProw := PRCardLine(st, PRCard{Check: "build-and-test"}, 40)
	if !strings.Contains(nonProw, "FAILED") || !strings.Contains(nonProw, "no Prow job") {
		t.Errorf("non-Prow check: %q", nonProw)
	}

	flaky := PRCardLine(st, PRCard{Job: job, Prow: true, Health: &health.Summary{
		Verdict: health.VerdictFlaky, PassRate: 0.5, Runs: 10,
	}}, 40)
	for _, want := range []string{"FLAKY", "50.0%", "10 runs"} {
		if !strings.Contains(flaky, want) {
			t.Errorf("flaky line missing %q: %q", want, flaky)
		}
	}

	// The job column is 40; the suffix must still fit the 100-column style.
	long := PRCardLine(st, PRCard{Job: strings.Repeat("x", 200), Prow: true}, 40)
	if n := term.Visible(strings.SplitN(long, "\n", 2)[0]); n > st.Width() {
		t.Errorf("line is %d columns wide: %q", n, long)
	}
}

func TestIsText(t *testing.T) {
	if !isText([]byte("hello\nworld")) {
		t.Error("plain text should be text")
	}
	if isText([]byte{'a', 0, 'b'}) {
		t.Error("NUL byte should be binary")
	}
}

func historyRecords() []stats.Record {
	return []stats.Record{
		{TS: time.Date(2026, 11, 15, 4, 38, 0, 0, time.UTC), Cmd: "log",
			Target:    "pr-logs/pull/rh-ecosystem-edge_recert/2364/job/2/artifacts/gather/build-log.txt",
			ReadBytes: 10_300, EmittedBytes: 210},
		{TS: time.Date(2026, 11, 15, 4, 37, 0, 0, time.UTC), Cmd: "run",
			Target:    "pull-ci-rh-ecosystem-edge-recert-main-e2e/2120987667142283264",
			ReadBytes: 11_200_000, EmittedBytes: 4_900},
		{TS: time.Date(2026, 11, 12, 22, 10, 0, 0, time.UTC), Cmd: "failures",
			Target:    "pull-ci-rh-ecosystem-edge-recert-main-e2e/2120987667142283264",
			ReadBytes: 5_500_000, EmittedBytes: 1_100},
		{TS: time.Date(2026, 11, 13, 9, 5, 0, 0, time.UTC), Cmd: "job",
			Target:    "pull-ci-rh-ecosystem-edge-recert-main-e2e-aws-ovn-single-node-recert-serial",
			ReadBytes: 105_000, EmittedBytes: 640, Verdict: "FLAKY", PassRate: 0.64},
	}
}

func TestHistoryGolden(t *testing.T) {
	var buf bytes.Buffer
	History(&buf, testStyle(), historyRecords())
	golden(t, "history.txt", buf.String())
}

func TestFunGolden(t *testing.T) {
	recs := historyRecords()
	recs = append(recs, stats.Record{
		TS: time.Date(2026, 11, 15, 4, 39, 0, 0, time.UTC), Cmd: "job",
		Target:    "pull-ci-rh-ecosystem-edge-recert-main-baremetalds-sno-recert-cluster-rename",
		ReadBytes: 260_000, EmittedBytes: 700, Verdict: "BROKEN", PassRate: 0.13,
	})
	var buf bytes.Buffer
	Fun(&buf, testStyle(), recs)
	golden(t, "fun.txt", buf.String())
}

func TestCallSummary(t *testing.T) {
	st := testStyle()
	if got := callSummary(st, nil); got != "" {
		t.Errorf("empty summary = %q", got)
	}
	// Four calls across three days, with a zero-call day in the middle: the
	// span counts days, not rows.
	recs := []stats.Record{
		{TS: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)},
		{TS: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)},
		{TS: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)},
		{TS: time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)},
	}
	if got, want := callSummary(st, recs), "4 over 3 days · busiest 15 Nov (3)"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	// One day is singular, not "1 days".
	if got, want := callSummary(st, recs[1:]), "3 over 1 day · busiest 15 Nov (3)"; got != want {
		t.Errorf("single day = %q, want %q", got, want)
	}
	// A month boundary is stated, not implied by a bare day number.
	across := []stats.Record{
		{TS: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)},
		{TS: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)},
	}
	if got, want := callSummary(st, across), "2 over 2 days · busiest 31 Oct (1)"; got != want {
		t.Errorf("month boundary = %q, want %q", got, want)
	}
}

func TestSearchGolden(t *testing.T) {
	var buf bytes.Buffer
	Search(&buf, testStyle(), 48,
		[]string{"pull-ci-org-repo-main-e2e-aws", "pull-ci-org-repo-main-e2e-gcp"}, "OCPBUGS-1234", "48h")
	golden(t, "search.txt", buf.String())
}

func TestArtifactListGolden(t *testing.T) {
	var buf bytes.Buffer
	ArtifactList(&buf, testStyle(), []ArtifactEntry{
		{Name: "artifacts/prowjob.json", Size: 12_300},
		{Name: "artifacts/ci-operator-step-graph.json", Size: 418_000},
	}, false)
	golden(t, "artifact-list.txt", buf.String())

	var truncated bytes.Buffer
	ArtifactList(&truncated, testStyle(), []ArtifactEntry{
		{Name: "artifacts/prowjob.json", Size: 12_300},
	}, true)
	golden(t, "artifact-list-truncated.txt", truncated.String())
}

func TestArtifactGolden(t *testing.T) {
	var log bytes.Buffer
	Artifact(&log, testStyle(), ArtifactInfo{
		Name: "run/artifacts/gather/build-log.txt", Size: 2048,
		ContentType: "text/plain", Updated: "2026-11-13T02:12:00Z",
		Data: []byte("line one\nline two\n"), IsLog: true,
	})
	golden(t, "artifact-log.txt", log.String())

	var excerpt bytes.Buffer
	Artifact(&excerpt, testStyle(), ArtifactInfo{
		Name: "run/artifacts/prowjob.json", Size: 99_999,
		ContentType: "application/json", Updated: "2026-11-13T02:12:00Z",
		Data: []byte(`{"kind":"ProwJob"}`), Truncated: true, Limit: 64 << 10,
	})
	golden(t, "artifact-excerpt.txt", excerpt.String())
}

func TestPRGolden(t *testing.T) {
	flaky := &health.Summary{Verdict: health.VerdictFlaky, PassRate: 0.638, Runs: 154}
	var buf bytes.Buffer
	PR(&buf, testStyle(), "rh-ecosystem-edge", "recert", "2364", "CNF-30065: add integration scenarios", []PRCard{
		{Check: "ci/prow/e2e-aws", Job: "pull-ci-rh-ecosystem-edge-recert-main-e2e-aws", Prow: true, Health: flaky},
		{Check: "ci/prow/missing", Job: "pull-ci-rh-ecosystem-edge-recert-main-missing", Prow: true},
		{Check: "lint"},
	})
	golden(t, "pr.txt", buf.String())
}

// TestRenderWidths is the regression guard for the whole ledger: no path may
// paint a line wider than the terminal it was given.
func TestRenderWidths(t *testing.T) {
	narrow := term.Config(new(bytes.Buffer), "never", 60)
	var buf bytes.Buffer
	History(&buf, narrow, historyRecords())
	Search(&buf, narrow, 3, []string{"pull-ci-org-repo-main-e2e-aws-with-a-very-long-name-that-goes-on"}, "", "48h")
	ArtifactList(&buf, narrow, []ArtifactEntry{{Name: "artifacts/" + strings.Repeat("x", 120), Size: 1024}}, false)
	Artifact(&buf, narrow, ArtifactInfo{
		Name: "run/artifacts/gather/build-log.txt", Size: 2048,
		ContentType: "text/plain", Updated: "2026-11-13T02:12:00Z",
		Data: []byte("line one\n"), IsLog: true,
	})
	PR(&buf, narrow, "org", "repo", "1", "a long enough title to fold", []PRCard{
		{Check: "ci/prow/x", Job: "pull-ci-org-repo-main-" + strings.Repeat("y", 80), Prow: true},
	})
	Gain(&buf, stats.Aggregate{
		Since:    time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Totals:   stats.Totals{Calls: 2, ReadBytes: 1000, EmittedBytes: 100, NetworkBytes: 300, InventoryBytes: 5000, Missing: 3},
		Groups:   map[string]stats.Totals{"rh-ecosystem-edge/recert-with-a-long-name": {Calls: 2, ReadBytes: 1000}},
		Top:      []stats.Record{{Cmd: "run", Target: "job/" + strings.Repeat("z", 90), ReadBytes: 1000, EmittedBytes: 100}},
		Verdicts: map[string]int{"FLAKY": 1, "BROKEN": 1},
	}, 4, 10*time.Minute, narrow)
	Fun(&buf, narrow, historyRecords())

	for _, line := range strings.Split(buf.String(), "\n") {
		if n := term.Visible(line); n > 60 {
			t.Errorf("line is %d columns wide: %q", n, line)
		}
	}
}

func TestNextMilestone(t *testing.T) {
	cases := []struct {
		saved int64
		want  int64
	}{
		{0, 0},              // nothing saved yet
		{-5, 0},             // a negative total is not worth celebrating
		{1, 1 << 20},        // first milestone is 1 MB
		{1 << 20, 10 << 20}, // exactly at one rolls to the next
		{10 << 20, 100 << 20},
		{100 << 20, 1 << 30},
		{1 << 30, 10 << 30},
		{100 << 30, 0}, // past the last milestone: nothing left to count to
	}
	for _, c := range cases {
		if got := nextMilestone(c.saved); got != c.want {
			t.Errorf("nextMilestone(%d) = %d, want %d", c.saved, got, c.want)
		}
	}
}

func TestVerdictCounts(t *testing.T) {
	st := testStyle()
	if got := verdictCounts(st, map[string]int{"FLAKY": 0}); got != "" {
		t.Errorf("zero counts rendered %q", got)
	}
	if got := verdictCounts(st, nil); got != "" {
		t.Errorf("nil counts rendered %q", got)
	}
	// Worst first, and a zero count is omitted rather than printed as 0 FLAKY.
	got := verdictCounts(st, map[string]int{"HEALTHY": 1, "BROKEN": 2, "FLAKY": 0})
	if got != "2 BROKEN · 1 HEALTHY" {
		t.Errorf("order/count = %q", got)
	}
	// A verdict this build does not know is shown, not silently dropped.
	got = verdictCounts(st, map[string]int{"FLAKY": 1, "WEIRD": 2})
	if got != "1 FLAKY · 2 WEIRD" {
		t.Errorf("unknown verdict = %q", got)
	}
}

func TestGroupBarWidth(t *testing.T) {
	if got := groupBarWidth(term.Config(new(bytes.Buffer), "never", 60)); got != 13 {
		t.Errorf("width 60 bar = %d", got)
	}
	// Wide terminals cap the bar rather than letting it grow without bound.
	if got := groupBarWidth(testStyle()); got != 24 {
		t.Errorf("width 100 bar = %d, want the 24 cap", got)
	}
	// Below the threshold there is no room for a bar at all.
	if got := groupBarWidth(term.Config(new(bytes.Buffer), "never", 30)); got != 0 {
		t.Errorf("width 30 bar = %d, want 0", got)
	}
}

func TestGrowWraps(t *testing.T) {
	// A value that does not fit folds under its own label and stays inside the
	// terminal; the long word is hard-split rather than overflowing.
	var buf bytes.Buffer
	narrow := term.Config(new(bytes.Buffer), "never", 40)
	grow(&buf, narrow, "read", "1000 B → 100 B emitted (10.0% of input), plus a verylongunbrokenwordthatexceedsthewidth")
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if n := term.Visible(line); n > 40 {
			t.Errorf("line is %d columns wide: %q", n, line)
		}
	}
	if got := buf.String(); !strings.Contains(got, "read") {
		t.Errorf("label lost: %q", got)
	}
}

func TestFunSkipsWhatItCannotSay(t *testing.T) {
	st := testStyle()

	var empty bytes.Buffer
	Fun(&empty, st, nil)
	if empty.Len() != 0 {
		t.Errorf("no records should print nothing: %q", empty.String())
	}

	// Records with no verdict and no pass rate: no roulette line.
	var plain bytes.Buffer
	Fun(&plain, st, []stats.Record{
		{TS: time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC), Cmd: "log", ReadBytes: 10},
	})
	out := plain.String()
	if strings.Contains(out, "roulette") {
		t.Errorf("roulette without judged records: %q", out)
	}
	if !strings.Contains(out, "worst") || !strings.Contains(out, "hours") {
		t.Errorf("missing fun lines: %q", out)
	}
}
