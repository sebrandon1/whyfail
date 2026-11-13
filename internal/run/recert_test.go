package run

import (
	"context"
	"strings"
	"testing"

	"github.com/sebrandon1/whyfail/internal/gcstest"
	"github.com/sebrandon1/whyfail/internal/prow"
)

// fixtureDir locates a corpus recorded by scripts/fetch-fixtures.sh, e.g.
// ("recert-2364", "ipc") or ("cert-manager-533", "e2e-operator").
func fixtureDir(t *testing.T, corpus, name string) string {
	t.Helper()
	return gcstest.RepoRoot(t) + "/testdata/" + corpus + "/jobs/" + name
}

// buildFixture loads one recorded run and assembles its digest exactly as the
// CLI does, with history disabled so the assertions stay on the run itself.
func buildFixture(t *testing.T, corpus, name string) (*Digest, *gcstest.Dataset) {
	t.Helper()
	d := gcstest.Load(t, fixtureDir(t, corpus, name))
	c := gcstest.Client(t, d)
	ref, err := prow.ParseRef("gs://" + gcstest.Bucket + "/" + d.RunPrefix())
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	dig, err := Build(context.Background(), c, ref, Options{History: false})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return dig, d
}

func TestRecertIPC(t *testing.T) {
	d, _ := buildFixture(t, "recert-2364", "ipc")
	assertIPC(t, d)
}

func assertIPC(t *testing.T, d *Digest) {
	t.Helper()

	if d.Run.Result != "FAILURE" || d.Run.State != "failure" {
		t.Errorf("result/state = %q/%q", d.Run.Result, d.Run.State)
	}
	if d.Run.JobName != "pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow" {
		t.Errorf("job = %q", d.Run.JobName)
	}
	if d.Run.RerunCommand != "/test ipc-e2e-flow" {
		t.Errorf("rerun = %q", d.Run.RerunCommand)
	}
	if pr := d.Run.PR; pr == nil || pr.Org != "rh-ecosystem-edge" || pr.Repo != "recert" || pr.Number != 2364 {
		t.Errorf("pr = %+v", pr)
	}
	if d.Run.Timing.TimeoutS != 28800 {
		t.Errorf("timeout = %v", d.Run.Timing.TimeoutS)
	}

	// The step graph has 18 nodes; junit_operator.xml contributes one more
	// failure, so the merged digest lists two.
	if d.Run.Steps.Total != 18 {
		t.Errorf("steps total = %d", d.Run.Steps.Total)
	}
	names := failedStepNames(d.Run.Steps.Failed)
	want := []string{"ipc-e2e-flow", "openshift-image-based-network-configuration-configure"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("failed steps = %v, want %v", names, want)
	}
	if !strings.Contains(d.Run.Steps.Failed[1].Error, "no route to host") {
		t.Errorf("step error = %q", d.Run.Steps.Failed[1].Error)
	}

	if d.Tests != nil {
		t.Errorf("expected no structured tests for this run, got %+v", d.Tests)
	}

	in := d.Infra
	if in == nil {
		t.Fatal("no infra")
	}
	if in.ClusterProfile != "openshift-org-aws" || in.Region != "aws-2" {
		t.Errorf("profile/region = %q/%q", in.ClusterProfile, in.Region)
	}
	if in.Lease == nil || in.Lease.Slice != "240" || in.Lease.AvailableAtAcquire != 0 {
		t.Errorf("lease = %+v", in.Lease)
	}
	if in.MachinesTotal != 10 {
		t.Errorf("machines = %d", in.MachinesTotal)
	}
	if in.Execution == nil || in.Execution.Success || in.Execution.ErrorCount != 1 {
		t.Errorf("execution = %+v", in.Execution)
	}

	if d.Artifacts == nil || d.Artifacts.Count != 72 || d.Artifacts.TotalBytes != 24052955 {
		t.Errorf("artifacts = %+v", d.Artifacts)
	}
	if !strings.Contains(d.NextAction, "openshift-image-based-network-configuration-configure") {
		t.Errorf("next action = %q", d.NextAction)
	}
}

func TestRecertSerial(t *testing.T) {
	d, _ := buildFixture(t, "recert-2364", "serial")
	assertSerial(t, d)
}

func assertSerial(t *testing.T, d *Digest) {
	t.Helper()

	if d.Run.Steps.Total != 17 {
		t.Errorf("steps total = %d", d.Run.Steps.Total)
	}
	if d.Tests == nil {
		t.Fatal("no tests")
	}
	c := d.Tests.Counts
	if c.Passed != 261 || c.Failed != 3 || c.Skipped != 497 {
		t.Errorf("counts = %d/%d/%d, want 261/3/497", c.Passed, c.Failed, c.Skipped)
	}
	if d.Tests.FailedTotal != 3 {
		t.Errorf("failed total = %d", d.Tests.FailedTotal)
	}
	if d.Tests.MonitorTotal != 86 {
		t.Errorf("monitor total = %d", d.Tests.MonitorTotal)
	}

	blocking := 0
	for _, f := range d.Tests.Failed {
		if f.Blocking {
			blocking++
			if !strings.Contains(f.Name, "Platform Prometheus targets") {
				t.Errorf("unexpected blocking test %q", f.Name)
			}
		}
	}
	if blocking != 1 {
		t.Errorf("blocking failures = %d, want 1", blocking)
	}

	// The digest keeps at most ten monitor representatives but reports the
	// true total, and causes are grouped and ordered by frequency.
	if len(d.Tests.Monitors) > 10 {
		t.Errorf("monitors not capped: %d", len(d.Tests.Monitors))
	}
	if len(d.Tests.Causes) == 0 {
		t.Fatal("no cause groups")
	}
	first := d.Tests.Causes[0]
	if first.Signature != "monitor: kubelet-container-restarts" || first.Count != 55 {
		t.Errorf("top cause = %+v", first)
	}

	if d.Artifacts.Count != 5439 || d.Artifacts.TotalBytes != 578650660 {
		t.Errorf("artifacts = %d/%d", d.Artifacts.Count, d.Artifacts.TotalBytes)
	}
}

func TestRecertBaremetal(t *testing.T) {
	d, _ := buildFixture(t, "recert-2364", "baremetal")
	assertBaremetal(t, d)
}

func assertBaremetal(t *testing.T, d *Digest) {
	t.Helper()

	if d.Run.Steps.Total != 13 {
		t.Errorf("steps total = %d", d.Run.Steps.Total)
	}
	if len(d.Run.Steps.Failed) != 3 {
		t.Errorf("failed steps = %v", failedStepNames(d.Run.Steps.Failed))
	}

	if d.Tests == nil {
		t.Fatal("no tests")
	}
	c := d.Tests.Counts
	if c.Passed != 50 || c.Failed != 0 {
		t.Errorf("counts = %d/%d, want 50/0", c.Passed, c.Failed)
	}
	if d.Tests.FailedTotal != 0 {
		t.Errorf("failed total = %d", d.Tests.FailedTotal)
	}
	if d.Tests.MonitorTotal != 76 {
		t.Errorf("monitor total = %d", d.Tests.MonitorTotal)
	}

	if d.Infra == nil || d.Infra.ClusterProfile != "equinix-edge-enablement" {
		t.Errorf("infra = %+v", d.Infra)
	}
	if d.Infra.MachinesTotal != 14 {
		t.Errorf("machines = %d", d.Infra.MachinesTotal)
	}
	if e := d.Infra.Execution; e == nil || e.Success || e.ErrorCount != 1 {
		t.Errorf("execution = %+v", e)
	}

	if d.Artifacts.Count != 4867 || d.Artifacts.TotalBytes != 60456149 {
		t.Errorf("artifacts = %d/%d", d.Artifacts.Count, d.Artifacts.TotalBytes)
	}
}

// TestRecertReadSet pins the objects a digest actually fetches. It is the
// guard against a change that starts pulling megabytes of artifacts.
func TestRecertReadSet(t *testing.T) {
	cases := map[string][]string{
		"ipc": {
			"prowjob.json",
			"finished.json",
			"artifacts/ci-operator-step-graph.json",
			"artifacts/junit_operator.xml",
			"build-log.txt",
			"artifacts/ci-operator-metrics.json",
		},
		"baremetal": {
			"prowjob.json",
			"finished.json",
			"artifacts/ci-operator-step-graph.json",
			"artifacts/junit_operator.xml",
			"build-log.txt",
			"artifacts/baremetalds-sno-recert-cluster-rename/baremetalds-sno-test/artifacts/tmp/artifacts/reports/extension_test_result_e2e__20261112-230557.json",
			"artifacts/baremetalds-sno-recert-cluster-rename/baremetalds-sno-test/artifacts/tmp/artifacts/reports/extension_test_result_monitors__20261112-230557.json",
			"artifacts/ci-operator-metrics.json",
		},
	}
	for name, want := range cases {
		_, ds := buildFixture(t, "recert-2364", name)
		got := ds.Requests()
		// Skip the listing call; the rest must match exactly, in order.
		if len(got) == 0 || !strings.HasPrefix(got[0], "list ") {
			t.Fatalf("%s: first request %v", name, got)
		}
		got = got[1:]
		if len(got) != len(want) {
			t.Fatalf("%s: read %d objects, want %d: %v", name, len(got), len(want), got)
		}
		for i := range want {
			if !strings.HasSuffix(got[i], "/"+want[i]) {
				t.Errorf("%s: read[%d] = %q, want suffix %q", name, i, got[i], want[i])
			}
		}
	}
}

// TestRecertMissingRunDegradesGracefully covers a run pruned from Prow: the
// listing comes back empty and every object 404s.
func TestRecertMissingRunDegradesGracefully(t *testing.T) {
	d := &gcstest.Dataset{
		Objects: map[string][]byte{},
		Listing: []gcstest.Item{{Name: "pr-logs/pull/x/1/job/999/prowjob.json", Size: "1"}},
	}
	c := gcstest.Client(t, d)
	ref, err := prow.ParseRef("gs://" + gcstest.Bucket + "/pr-logs/pull/x/1/job/999")
	if err != nil {
		t.Fatal(err)
	}
	dig, err := Build(context.Background(), c, ref, Options{History: false})
	if err != nil {
		t.Fatalf("a pruned run must not fail the digest: %v", err)
	}
	if dig.Artifacts != nil && dig.Artifacts.Count != 1 {
		t.Errorf("artifacts = %+v", dig.Artifacts)
	}
	if dig.Run.Result != "" {
		t.Errorf("result = %q", dig.Run.Result)
	}
	if c.Missing == 0 {
		t.Error("expected missing-object count to be tracked")
	}
}

func failedStepNames(steps []StepRef) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}
