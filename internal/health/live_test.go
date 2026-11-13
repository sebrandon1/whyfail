//go:build live

package health

import (
	"context"
	"testing"

	"github.com/sebrandon1/whyfail/internal/gcs"
	"github.com/sebrandon1/whyfail/internal/prow"
)

// TestLiveRecertHistory checks the presubmit run index against the real
// bucket. The exact counts are not pinned here because more runs may appear;
// recert_test.go pins the recorded snapshot.
func TestLiveRecertHistory(t *testing.T) {
	c := gcs.New(t.TempDir())
	sum, err := Collect(context.Background(), c, prow.PublicBucket, recertJob, 0)
	if err != nil {
		t.Fatalf("collect: %v (network failure, not necessarily drift)", err)
	}
	if sum.Runs == 0 {
		t.Fatalf("no runs indexed for %s", recertJob)
	}
	if sum.Pass+sum.Fail+sum.Abort+sum.Missing != sum.Runs {
		t.Errorf("counts do not add up: %+v", sum)
	}
	if sum.Verdict == VerdictUnknown {
		t.Errorf("verdict = %q with %d runs", sum.Verdict, sum.Runs)
	}
}
