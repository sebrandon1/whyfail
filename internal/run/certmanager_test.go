package run

import (
	"strings"
	"testing"
)

// TestCertManagerE2EOperator covers the one failing Prow check on
// https://github.com/openshift/cert-manager-operator/pull/533. Unlike the
// recert runs this job emits a bare artifacts/junit.xml and no extension
// result JSON, so it exercises the JUnit fallback end to end.
func TestCertManagerE2EOperator(t *testing.T) {
	d, _ := buildFixture(t, "cert-manager-533", "e2e-operator")
	assertCertManager(t, d)
}

func assertCertManager(t *testing.T, d *Digest) {
	t.Helper()

	if d.Run.JobName != "pull-ci-openshift-cert-manager-operator-master-e2e-operator" {
		t.Errorf("job = %q", d.Run.JobName)
	}
	if d.Run.Result != "FAILURE" || d.Run.State != "failure" {
		t.Errorf("result/state = %q/%q", d.Run.Result, d.Run.State)
	}
	pr := d.Run.PR
	if pr == nil || pr.Org != "openshift" || pr.Repo != "cert-manager-operator" || pr.Number != 533 {
		t.Fatalf("pr = %+v", pr)
	}
	if pr.Author != "sebrandon1" || pr.BaseRef != "master" {
		t.Errorf("author/base = %q/%q", pr.Author, pr.BaseRef)
	}

	if d.Run.Steps.Total != 14 {
		t.Errorf("steps total = %d, want 14", d.Run.Steps.Total)
	}
	names := failedStepNames(d.Run.Steps.Failed)
	if len(names) == 0 || names[0] != "e2e-operator" {
		t.Errorf("failed steps = %v", names)
	}

	if d.Tests == nil {
		t.Fatal("no tests: the bare junit.xml was not parsed")
	}
	if d.Tests.Source != "junit" {
		t.Errorf("source = %q, want junit", d.Tests.Source)
	}
	c := d.Tests.Counts
	if c.Passed != 77 || c.Failed != 1 || c.Skipped != 156 {
		t.Errorf("counts = %d/%d/%d, want 77/1/156", c.Passed, c.Failed, c.Skipped)
	}
	if d.Tests.FailedTotal != 1 {
		t.Errorf("failed total = %d, want 1", d.Tests.FailedTotal)
	}
	if len(d.Tests.Failed) != 1 || !strings.Contains(d.Tests.Failed[0].Name, "Istio-CSR operand coverage") {
		t.Errorf("failed tests = %+v", d.Tests.Failed)
	}

	if d.Infra == nil {
		t.Fatal("no infra")
	}
	// This ran on a shared build cluster, so there is no provisioned cluster
	// profile and no lease; the execution insight is still there.
	if d.Infra.ClusterProfile != "" || d.Infra.Lease != nil {
		t.Errorf("expected no profile/lease for a build-cluster run, got %+v", d.Infra)
	}
	if e := d.Infra.Execution; e == nil || e.Success || e.ErrorCount != 1 {
		t.Errorf("execution = %+v", e)
	}

	if d.Artifacts == nil || d.Artifacts.Count != 6276 || d.Artifacts.TotalBytes != 969699738 {
		t.Errorf("artifacts = %+v", d.Artifacts)
	}
}
