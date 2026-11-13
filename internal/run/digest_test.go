package run

import (
	"strings"
	"testing"

	"github.com/sebrandon1/whyfail/internal/gcs"
)

func TestMonitorSignature(t *testing.T) {
	thanos := "failed during collection\nquery returned error: Post \"https://thanos-querier-openshift-monitoring.apps.x/api/v1/query_range\": dial tcp: lookup thanos-querier-openshift-monitoring.apps.x on 127.0.0.1:53: no such host"
	if got := monitorSignature("[Monitor:api-unreachable] x", thanos); got != "dns lookup failed: thanos-querier-openshift-monitoring.apps.x" {
		t.Fatalf("got %q", got)
	}

	annotation := "annotation missing from pod 'dns-default-abc' (owners: daemonset/dns-default)"
	if got := monitorSignature("[Monitor:required-scc-annotation-checker][sig-auth] x", annotation); got != "monitor: required-scc-annotation-checker" {
		t.Fatalf("got %q", got)
	}
}

func TestStepErrorSkipsProgress(t *testing.T) {
	in := "......................................................................... TIMEOUT\nUnable to connect to the server: dial tcp 192.168.1.1:6443: connect: no route to host\nmake[1]: *** [x] Error 1"
	if got := stepError(in); !strings.Contains(got, "no route to host") {
		t.Fatalf("got %q", got)
	}
}

func TestSignatureStripsVolatileTokens(t *testing.T) {
	got := signatureOf("dial tcp 10.0.0.1:6443 in ci-op-abcdefgh pod foo-abcde-12345 at 2026-11-13T00:00:00Z")
	for _, want := range []string{"<ip>", "ci-op-<ns>", "-<pod>", "<ts>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestSummarizeStepsConvertsNanoseconds(t *testing.T) {
	s := summarizeSteps([]step{{Name: "a", Duration: 2.5e9}, {Name: "b", Duration: 1e9, Failed: true}})
	if s.Total != 2 || len(s.Failed) != 1 || s.Failed[0].DurationS != 1 {
		t.Fatalf("got %+v", s)
	}
	if s.Longest[0].Name != "a" || s.Longest[0].DurationS != 2.5 {
		t.Fatalf("got %+v", s.Longest)
	}
}

func TestParseJUnit(t *testing.T) {
	xmlDoc := `<testsuite>` +
		`<testcase name="Run multi-stage step foo" time="12.5"><failure message="m">boom</failure></testcase>` +
		`<testcase name="ok"/>` +
		`<testcase name="skipped one"><skipped/></testcase>` +
		`<testcase name="errored"><error message="e">bad</error></testcase>` +
		`</testsuite>`
	r, err := parseJUnit(strings.NewReader(xmlDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failures) != 2 || r.Failures[0].Name != "Run multi-stage step foo" || r.Failures[0].DurationMS != 12500 {
		t.Fatalf("got %+v", r.Failures)
	}
	if !strings.Contains(r.Failures[0].Error, "boom") {
		t.Fatalf("got %q", r.Failures[0].Error)
	}
	if r.Total != 4 || r.Failed != 2 || r.Skipped != 1 || r.Passed() != 1 {
		t.Fatalf("counts = %+v, passed=%d", r, r.Passed())
	}
}

func TestJUnitCandidatesPrefersNamed(t *testing.T) {
	objs := []gcs.Object{
		{Name: "pr-logs/x/1/job/2/artifacts/e2e-operator/test/artifacts/junit.xml"},
		{Name: "pr-logs/x/1/job/2/artifacts/step/artifacts/junit_e2e__1.xml"},
	}
	got := junitCandidates(objs)
	if len(got) != 1 || !strings.HasSuffix(got[0].Name, "junit_e2e__1.xml") {
		t.Fatalf("got %+v", got)
	}

	bare := junitCandidates(objs[:1])
	if len(bare) != 1 || !strings.HasSuffix(bare[0].Name, "junit.xml") {
		t.Fatalf("bare: got %+v", bare)
	}
}

func TestGroupCauses(t *testing.T) {
	fs := []TestFailure{{Signature: "a"}, {Signature: "a"}, {Signature: "b"}}
	cs := GroupCauses(fs)
	if len(cs) != 2 || cs[0].Count != 2 || cs[0].Signature != "a" {
		t.Fatalf("got %+v", cs)
	}
}
