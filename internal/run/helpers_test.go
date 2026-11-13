package run

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebrandon1/whyfail/internal/gcs"
	"github.com/sebrandon1/whyfail/internal/health"
)

func TestApplyProwJob(t *testing.T) {
	raw := `{
	  "spec": {
	    "type": "presubmit",
	    "cluster": "build01",
	    "namespace": "ci-op-abcdefgh",
	    "job": "pull-ci-org-repo-main-e2e",
	    "rerun_command": "/test e2e",
	    "refs": {
	      "org": "org", "repo": "repo", "base_ref": "main", "base_sha": "base",
	      "pulls": [{"number": 2364, "author": "alice", "sha": "head", "title": "fix it"}]
	    },
	    "decoration_config": {"timeout": "8h"}
	  },
	  "status": {
	    "state": "failure",
	    "pendingTime": "2026-11-13T00:00:00Z",
	    "startTime": "2026-11-13T00:10:00Z",
	    "completionTime": "2026-11-13T01:10:00Z"
	  }
	}`
	var pj prowJob
	if err := json.Unmarshal([]byte(raw), &pj); err != nil {
		t.Fatal(err)
	}
	var r RunInfo
	applyProwJob(&r, pj)

	if r.JobName != "pull-ci-org-repo-main-e2e" || r.Type != "presubmit" || r.State != "failure" {
		t.Fatalf("identity: %+v", r)
	}
	if r.Cluster != "build01" || r.Namespace != "ci-op-abcdefgh" || r.RerunCommand != "/test e2e" {
		t.Fatalf("cluster/namespace: %+v", r)
	}
	if r.PR == nil || r.PR.Number != 2364 || r.PR.Author != "alice" || r.PR.SHA != "head" || r.PR.BaseRef != "main" {
		t.Fatalf("pr: %+v", r.PR)
	}
	if r.Timing.TimeoutS != 8*3600 {
		t.Fatalf("timeout: %v", r.Timing.TimeoutS)
	}
	if r.Timing.DurationS != 3600 {
		t.Fatalf("duration: %v", r.Timing.DurationS)
	}
	if r.Timing.PctOfTimeout < 12.4 || r.Timing.PctOfTimeout > 12.6 {
		t.Fatalf("pct: %v", r.Timing.PctOfTimeout)
	}
}

func TestApplyProwJobPeriodicHasNoPR(t *testing.T) {
	var pj prowJob
	pj.Spec.Job = "periodic-x"
	var r RunInfo
	applyProwJob(&r, pj)
	if r.PR != nil {
		t.Fatalf("expected no PR, got %+v", r.PR)
	}
	if r.JobName != "periodic-x" {
		t.Fatalf("job: %q", r.JobName)
	}
}

func TestBuildReason(t *testing.T) {
	log := []byte(`Reporting job state 'pending' with reason 'executing_graph'
Reporting job state 'failure' with reason 'executing_graph:step_failed:utilizing_lease'`)
	if got := buildReason(log); got != "executing_graph:step_failed:utilizing_lease" {
		t.Fatalf("got %q", got)
	}
	if got := buildReason([]byte("no such line")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRepresentatives(t *testing.T) {
	fs := []TestFailure{
		{Signature: "a", Error: strings.Repeat("x", 300)},
		{Signature: "a"},
		{Signature: "b"},
		{Signature: "c"},
	}
	got := representatives(fs, 2)
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].Signature != "a" || got[1].Signature != "b" {
		t.Fatalf("got %+v", got)
	}
	if len(got[0].Error) != 253 || !strings.HasSuffix(got[0].Error, "...") {
		t.Fatalf("error not trimmed: len=%d", len(got[0].Error))
	}
}

func TestFailureOfMarksBlocking(t *testing.T) {
	b := failureOf(testResult{Name: "n", Lifecycle: "blocking", Duration: 1500, Error: "boom"})
	if !b.Blocking || b.DurationMS != 1500 || b.Error != "boom" {
		t.Fatalf("got %+v", b)
	}
	i := failureOf(testResult{Name: "n", Lifecycle: "informing"})
	if i.Blocking {
		t.Fatalf("informing should not block: %+v", i)
	}
}

func TestNormalizeExcerpt(t *testing.T) {
	many := strings.Repeat("line\n", 10)
	if got := normalizeExcerpt(many); strings.Count(got, "\n") != 5 {
		t.Fatalf("expected 6 lines, got %d newlines", strings.Count(got, "\n"))
	}
	long := strings.Repeat("z", 700)
	if got := normalizeExcerpt(long); len(got) != 603 || !strings.HasSuffix(got, "...") {
		t.Fatalf("len=%d", len(got))
	}
	if got := normalizeExcerpt("  a\t\tb  "); got != "a b" {
		t.Fatalf("whitespace: %q", got)
	}
}

func TestJUnitAndMonitorCandidates(t *testing.T) {
	objs := []gcs.Object{
		{Name: "artifacts/junit_e2e__123.xml"},
		{Name: "artifacts/junit_install.xml"},
		{Name: "artifacts/junit_operator.xml"},
		{Name: "artifacts/e2e-monitor-tests__456.xml"},
		{Name: "artifacts/other.xml"},
	}
	j := junitCandidates(objs)
	if len(j) != 2 || !strings.HasSuffix(j[0].Name, "junit_e2e__123.xml") {
		t.Fatalf("junit: %+v", j)
	}
	m := monitorXML(objs)
	if len(m) != 1 || !strings.Contains(m[0].Name, "e2e-monitor-tests") {
		t.Fatalf("monitors: %+v", m)
	}
}

func TestLargest(t *testing.T) {
	if _, ok := largest(nil); ok {
		t.Fatal("empty should be false")
	}
	o, ok := largest([]gcs.Object{{Name: "a", Size: 1}, {Name: "b", Size: 9}, {Name: "c", Size: 3}})
	if !ok || o.Name != "b" {
		t.Fatalf("got %+v %v", o, ok)
	}
}

func TestInventory(t *testing.T) {
	base := "pr-logs/pull/o_r/1/job/2123/"
	objs := []gcs.Object{
		{Name: base + "prowjob.json", Size: 100},
		{Name: base + "build-log.txt", Size: 200},
		{Name: base + "artifacts/ci-operator-step-graph.json", Size: 50},
		{Name: base + "artifacts/e2e-flow/build-log.txt", Size: 500},
		{Name: base + "artifacts/install/build-log.txt", Size: 700},
		{Name: base + "artifacts/gather-must-gather/must-gather.tar", Size: 900},
		{Name: base + "artifacts/gather-extra/core.tar", Size: 900},
		{Name: base + "artifacts/noise.txt", Size: 10},
	}
	a := inventory(objs, base, []StepRef{{Name: "e2e-flow"}})

	if a.Count != len(objs) || a.TotalBytes != 3360 {
		t.Fatalf("totals: %+v", a)
	}
	got := map[string]string{}
	for _, n := range a.Notable {
		got[n.Short] = n.Why
	}
	if got["prowjob.json"] != "run identity and PR linkage" {
		t.Errorf("prowjob: %+v", got)
	}
	if got["artifacts/e2e-flow/build-log.txt"] != "failing step log" {
		t.Errorf("failed step log: %+v", got)
	}
	if _, ok := got["artifacts/install/build-log.txt"]; ok {
		t.Errorf("non-failed step log should not be notable: %+v", got)
	}
	if _, ok := got["artifacts/gather-extra/core.tar"]; ok {
		t.Errorf("gather duplicates should be skipped: %+v", got)
	}
	if len(a.Notable) > 12 {
		t.Errorf("too many notable: %d", len(a.Notable))
	}
}

func TestNextAction(t *testing.T) {
	broken := &Digest{
		Job: &JobInfo{Health: &health.Summary{Verdict: health.VerdictBroken}},
		Run: &RunInfo{State: "failure"}, Tests: &Tests{},
	}
	if got := nextAction(broken); !strings.Contains(got, "broken for everyone") {
		t.Errorf("broken: %q", got)
	}

	flaky := &Digest{
		Job: &JobInfo{Health: &health.Summary{Verdict: health.VerdictFlaky}},
		Run: &RunInfo{State: "failure"}, Tests: &Tests{},
	}
	if got := nextAction(flaky); !strings.Contains(got, "known flaky") {
		t.Errorf("flaky: %q", got)
	}

	blocking := &Digest{
		Run:   &RunInfo{State: "failure"},
		Tests: &Tests{Failed: []TestFailure{{Name: "t", Blocking: true}}},
	}
	if got := nextAction(blocking); !strings.Contains(got, "blocking test failed") {
		t.Errorf("blocking: %q", got)
	}

	step := &Digest{
		Run: &RunInfo{State: "failure", Steps: StepsInfo{Failed: []StepRef{{Name: "s1"}, {Name: "s2"}}}},
	}
	if got := nextAction(step); !strings.Contains(got, `step "s2" failed`) || !strings.Contains(got, "whyfail log") {
		t.Errorf("step: %q", got)
	}

	inform := &Digest{
		Run:   &RunInfo{State: "failure"},
		Tests: &Tests{Failed: []TestFailure{{Name: "t"}}},
	}
	if got := nextAction(inform); !strings.Contains(got, "tests failed") {
		t.Errorf("inform: %q", got)
	}

	if got := nextAction(&Digest{Run: &RunInfo{State: "failure"}}); !strings.Contains(got, "no clear cause") {
		t.Errorf("default: %q", got)
	}
	if got := nextAction(&Digest{Run: &RunInfo{State: "SUCCESS"}}); !strings.Contains(got, "nothing to do") {
		t.Errorf("success: %q", got)
	}
	if got := nextAction(&Digest{}); got != "" {
		t.Errorf("nil run: %q", got)
	}
}

func TestBlockingFailures(t *testing.T) {
	var nilTests *Tests
	if got := nilTests.blockingFailures(); got != nil {
		t.Errorf("nil: %+v", got)
	}
	ts := &Tests{Failed: []TestFailure{{Name: "a", Blocking: true}, {Name: "b"}}}
	got := ts.blockingFailures()
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("got %+v", got)
	}
}
