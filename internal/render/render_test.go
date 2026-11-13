package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/run"
	"github.com/sebrandon1/whyfail/internal/stats"
)

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
	if got := truncate("short", 10); got != "short" {
		t.Errorf("under limit: %q", got)
	}
	if got := truncate("hello world", 5); got != "hello…" {
		t.Errorf("over limit: %q", got)
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

func TestDigestRendersFullRun(t *testing.T) {
	now := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)
	d := &run.Digest{
		Schema: "whyfail/v1",
		Job: &run.JobInfo{Name: "pull-ci-org-repo-main-e2e", Health: &health.Summary{
			Job: "pull-ci-org-repo-main-e2e", Pass: 19, Fail: 122, Abort: 34, Runs: 175,
			PassRate: 0.135, From: now.AddDate(0, 0, -60), Verdict: health.VerdictFlaky,
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
			Notable: []run.Notable{{Short: "build-log.txt", Bytes: 2048, Why: "orchestration timeline"}},
		},
		Signals: &run.Signals{
			Sippy:    "not tracked",
			SearchCI: &run.SearchSig{Matches48h: 7, KnownBug: "OCPBUGS-1"},
		},
		NextAction: "retest",
	}

	var buf bytes.Buffer
	Digest(&buf, d)
	out := buf.String()

	for _, want := range []string{
		"FLAKY", "2123", "FAILURE  5m00s", "50% of 10m00s timeout",
		"org/repo#2364", "by alice", `"fix"`, "cluster build01", "ns ci-op-abcdefgh",
		"reason: step failed", "steps (19): 1 failed", "e2e-flow", "longest: install 2m00s",
		"tests: 247 pass / 3 fail / 497 skip / 1 flaked", "[extension]",
		"BLOCK", "[Late] Prometheus targets", "inform", "informing test",
		"monitors: 4 failures in 1 cause(s)", "[4x] dns lookup failed: thanos",
		"infra: profile=ipi/aws", "slice=240", "machines=6", "INSTALL FAILED",
		"ci-operator: success=false errors=1", "artifacts: 42 objects",
		"build-log.txt", "sippy=not tracked", "search.ci=7 matches in 48h", "OCPBUGS-1",
		"next: retest", "url:  https://prow.ci.openshift.org/view/gs/b/p",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest output missing %q\n---\n%s", want, out)
		}
	}
}

func TestDigestMinimalRun(t *testing.T) {
	d := &run.Digest{
		Run: &run.RunInfo{ID: "1", JobName: "periodic-x", Result: "success",
			URL: "https://prow.ci.openshift.org/view/gs/b/p"},
	}
	var buf bytes.Buffer
	Digest(&buf, d)
	out := buf.String()
	if !strings.HasPrefix(out, "periodic-x\n") {
		t.Errorf("expected bare job name header, got %q", out)
	}
	if strings.Contains(out, "tests:") || strings.Contains(out, "steps") {
		t.Errorf("unexpected optional sections: %q", out)
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
	Digest(&buf, d)
	if !strings.Contains(buf.String(), "monitors: 2 failures in 1 cause(s)") {
		t.Errorf("got %q", buf.String())
	}
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
		Job(&buf, &c.s)
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("%s: missing %q in %q", c.name, c.want, buf.String())
		}
	}
}

func TestGainEmptyAndPopulated(t *testing.T) {
	var buf bytes.Buffer
	Gain(&buf, stats.Aggregate{}, 4, 0)
	if !strings.Contains(buf.String(), "no recorded calls yet") {
		t.Fatalf("empty: %q", buf.String())
	}

	a := stats.Aggregate{
		Since: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Totals: stats.Totals{
			Calls: 2, ReadBytes: 1000, EmittedBytes: 100,
			NetworkBytes: 1000, InventoryBytes: 5000, CacheHits: 1, WallMS: 1000,
		},
		Groups: map[string]stats.Totals{"run": {Calls: 2, ReadBytes: 1000}},
		Top:    []stats.Record{{Cmd: "run", Target: "job/1", ReadBytes: 1000, EmittedBytes: 100}},
	}
	buf.Reset()
	Gain(&buf, a, 4, 10*time.Minute)
	out := buf.String()
	for _, want := range []string{
		"saved", "2 calls", "since 2026-10-14", "10.0% of input",
		"~tokens saved", "run", "top saves", "job/1",
		"manual time saved",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
