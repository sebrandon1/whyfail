package health

import (
	"context"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/gcstest"
)

const recertJob = "pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow"

// TestRecertHistory exercises the presubmit run index for the recert PR:
// pr-logs/directory/<job>/*.txt pointers plus each run's finished.json.
func TestRecertHistory(t *testing.T) {
	dir := gcstest.RepoRoot(t) + "/testdata/recert-2364/health/" + recertJob
	d := gcstest.Load(t, dir)
	c := gcstest.Client(t, d)

	sum, err := Collect(context.Background(), c, gcstest.Bucket, recertJob, 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	t.Logf("runs=%d pass=%d fail=%d abort=%d missing=%d rate=%.3f verdict=%s",
		sum.Runs, sum.Pass, sum.Fail, sum.Abort, sum.Missing, sum.PassRate, sum.Verdict)

	// 140 index entries minus latest-build.txt.
	if sum.Runs != 139 {
		t.Errorf("runs = %d, want 139", sum.Runs)
	}
	if sum.Pass+sum.Fail+sum.Abort+sum.Missing != sum.Runs {
		t.Errorf("counts do not add up: %+v", sum)
	}
	// The corpus includes a run whose finished.json says "aborted" in lower
	// case; it must be classified by value, not spelling, and land in Abort.
	if sum.Pass != 89 || sum.Fail != 21 || sum.Abort != 29 || sum.Missing != 0 {
		t.Errorf("counts = %d/%d/%d/%d, want 89/21/29/0", sum.Pass, sum.Fail, sum.Abort, sum.Missing)
	}
	if sum.Verdict != VerdictFlaky {
		t.Errorf("verdict = %q, want FLAKY", sum.Verdict)
	}
	if sum.DistinctPRs != 10 {
		t.Errorf("distinct PRs = %d, want 10", sum.DistinctPRs)
	}
	if sum.From.IsZero() || sum.To.IsZero() {
		t.Errorf("window = %v..%v", sum.From, sum.To)
	}
}

func TestRecertHistoryWindow(t *testing.T) {
	dir := gcstest.RepoRoot(t) + "/testdata/recert-2364/health/" + recertJob
	d := gcstest.Load(t, dir)
	c := gcstest.Client(t, d)

	full, err := Collect(context.Background(), c, gcstest.Bucket, recertJob, 0)
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := Collect(context.Background(), c, gcstest.Bucket, recertJob, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if narrow.Runs > full.Runs {
		t.Errorf("narrow window has more runs (%d) than the full one (%d)", narrow.Runs, full.Runs)
	}
	if narrow.Runs == 0 {
		t.Error("a 7-day window should still cover the captured run")
	}
}
