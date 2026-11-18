package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/gcs"
	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/prow"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeGCS serves a listing and object reads from a name→body map.
func fakeGCS(t *testing.T, objects map[string]string) *gcs.Client {
	t.Helper()
	c := gcs.New(t.TempDir())
	c.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/storage/v1/") {
			prefix := r.URL.Query().Get("prefix")
			var names []string
			for n := range objects {
				if strings.HasPrefix(n, prefix) {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			items := make([]map[string]string, 0, len(names))
			for _, n := range names {
				items = append(items, map[string]string{
					"name": n, "size": fmt.Sprint(len(objects[n])), "updated": "2026-11-13T00:00:00Z",
				})
			}
			body, _ := json.Marshal(map[string]any{"items": items})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
		}
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), "bkt/")
		if body, ok := objects[key]; ok {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("missing")), Header: http.Header{}}, nil
	})}
	return c
}

func TestBuildAssemblesDigest(t *testing.T) {
	const base = "pr-logs/pull/org_repo/2364/pull-ci-org-repo-main-e2e/2123/"
	objects := map[string]string{
		base + "prowjob.json": `{
		  "spec": {"type":"presubmit","cluster":"build01","namespace":"ci-op-abcdefgh",
		    "job":"pull-ci-org-repo-main-e2e","rerun_command":"/test e2e",
		    "refs":{"org":"org","repo":"repo","base_ref":"main","base_sha":"b",
		      "pulls":[{"number":2364,"author":"alice","sha":"h","title":"fix"}]},
		    "decoration_config":{"timeout":"8h"}},
		  "status":{"state":"failure","startTime":"2026-11-13T00:00:00Z","completionTime":"2026-11-13T01:00:00Z"}}`,
		base + "finished.json": `{"result":"FAILURE"}`,
		base + "build-log.txt": "Reporting job state 'failure' with reason 'executing_graph:step_failed:utilizing_lease'\n",
		base + "artifacts/ci-operator-step-graph.json": `[
		  {"name":"install","duration":7200e9,"failed":false},
		  {"name":"flow","duration":3600e9,"failed":true}]`,
		base + "artifacts/junit_operator.xml": `<testsuite>
		  <testcase name="Run multi-stage step child" time="1.5">
		    <failure message="x">Unable to connect to the server: dial tcp: no route to host</failure>
		  </testcase></testsuite>`,
		base + "artifacts/extension_test_result_e2e__1.json": `[
		  {"name":"ok","lifecycle":"informing","result":"passed","duration":5},
		  {"name":"blocker","lifecycle":"blocking","result":"failed","duration":10,"error":"boom"},
		  {"name":"skipped","lifecycle":"informing","result":"skipped"},
		  {"name":"flaky-ish","lifecycle":"informing","result":"flaked"}]`,
		base + "artifacts/extension_test_result_monitors__1.json": `[
		  {"name":"[Monitor:api] x","lifecycle":"informing","result":"failed",
		   "error":"dial tcp: lookup thanos-querier.apps.x: no such host"}]`,
		base + "artifacts/ci-operator-metrics.json": `{
		  "test_platform_insights":[
		    {"name":"configuration","additional_context":{"variant":"amd64",
		      "cluster_info":{"cluster_id":"cid","cluster_profiles":[{"profile_name":"aws"}]}}},
		    {"name":"execution_completed","additional_context":{"duration_seconds":3600,"error_count":1,"success":false}}],
		  "leases":[{"name":"l","slice":"240","region":"us-east-1",
		    "acquisition_duration_seconds":1.5,"leases_remaining_at_acquisition":0}],
		  "machines":[{"total_machines":6}]}`,
	}

	c := fakeGCS(t, objects)
	ref := prow.Ref{Bucket: "bkt", ObjectPath: strings.TrimSuffix(base, "/"), Job: "pull-ci-org-repo-main-e2e", RunID: "2123"}

	d, err := Build(context.Background(), c, ref, Options{History: true})
	if err != nil {
		t.Fatal(err)
	}

	if d.Schema != "whyfail/v1" || d.Run.Result != "FAILURE" || d.Run.State != "failure" {
		t.Fatalf("run: %+v", d.Run)
	}
	if d.Run.PR == nil || d.Run.PR.Number != 2364 || d.Run.PR.Author != "alice" {
		t.Fatalf("pr: %+v", d.Run.PR)
	}
	if d.Run.Timing.TimeoutS != 8*3600 || d.Run.Timing.DurationS != 3600 {
		t.Fatalf("timing: %+v", d.Run.Timing)
	}
	if !strings.Contains(d.Run.Reason, "step_failed") {
		t.Fatalf("reason: %q", d.Run.Reason)
	}

	if d.Run.Steps.Total != 2 {
		t.Fatalf("steps: %+v", d.Run.Steps)
	}
	var failed []string
	for _, s := range d.Run.Steps.Failed {
		failed = append(failed, s.Name)
	}
	if strings.Join(failed, ",") != "flow,child" {
		t.Fatalf("failed steps: %v (want flow then merged child)", failed)
	}
	if !strings.Contains(d.Run.Steps.Failed[1].Error, "no route to host") {
		t.Fatalf("child error: %q", d.Run.Steps.Failed[1].Error)
	}

	if d.Tests == nil {
		t.Fatal("expected tests")
	}
	if d.Tests.Source != "extension" || d.Tests.Counts.Passed != 1 || d.Tests.Counts.Failed != 1 ||
		d.Tests.Counts.Skipped != 1 || d.Tests.Counts.Flaked != 1 {
		t.Fatalf("counts: %+v", d.Tests)
	}
	if len(d.Tests.Failed) != 1 || !d.Tests.Failed[0].Blocking {
		t.Fatalf("failed tests: %+v", d.Tests.Failed)
	}
	if d.Tests.MonitorTotal != 1 || len(d.Tests.Causes) != 1 {
		t.Fatalf("monitors: total=%d causes=%+v", d.Tests.MonitorTotal, d.Tests.Causes)
	}
	if !strings.Contains(d.Tests.Causes[0].Signature, "thanos") {
		t.Fatalf("monitor cause: %+v", d.Tests.Causes)
	}

	if d.Infra == nil || d.Infra.ClusterProfile != "aws" || d.Infra.Variant != "amd64" {
		t.Fatalf("infra: %+v", d.Infra)
	}
	if d.Infra.Lease == nil || d.Infra.Lease.Slice != "240" || d.Infra.MachinesTotal != 6 {
		t.Fatalf("infra lease/machines: %+v", d.Infra)
	}
	if d.Infra.Execution == nil || d.Infra.Execution.Success {
		t.Fatalf("execution: %+v", d.Infra.Execution)
	}

	if d.Artifacts == nil || d.Artifacts.Count != len(objects) {
		t.Fatalf("artifacts: %+v", d.Artifacts)
	}
	if d.Job == nil || d.Job.Name != "pull-ci-org-repo-main-e2e" {
		t.Fatalf("job: %+v", d.Job)
	}
	if d.Job.Health == nil {
		t.Fatal("expected health even with no indexed runs")
	}
	if !strings.Contains(d.NextAction, `blocking test "blocker" failed`) {
		t.Fatalf("next: %q", d.NextAction)
	}
}

func TestBuildJUnitFallback(t *testing.T) {
	const base = "logs/periodic/1/"
	objects := map[string]string{
		base + "finished.json": `{"result":"FAILURE"}`,
		base + "artifacts/junit_e2e__9.xml": `<testsuite>
		  <testcase name="failing test" time="2"><failure>junit boom</failure></testcase></testsuite>`,
		base + "artifacts/e2e-monitor-tests__9.xml": `<testsuite>
		  <testcase name="monitor test" time="1"><failure>monitor boom</failure></testcase></testsuite>`,
	}
	c := fakeGCS(t, objects)
	ref := prow.Ref{Bucket: "bkt", ObjectPath: strings.TrimSuffix(base, "/"), Job: "periodic", RunID: "1"}

	d, err := Build(context.Background(), c, ref, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Tests == nil || d.Tests.Source != "junit" {
		t.Fatalf("tests: %+v", d.Tests)
	}
	if d.Tests.Counts.Failed != 1 || d.Tests.Failed[0].Name != "failing test" {
		t.Fatalf("junit failed: %+v", d.Tests.Failed)
	}
	if len(d.Tests.Monitors) != 1 || !d.Tests.Monitors[0].Blocking {
		t.Fatalf("monitor fallback: %+v", d.Tests.Monitors)
	}
	if d.Run.State != "failure" {
		t.Fatalf("state from finished result: %q", d.Run.State)
	}
}

func TestBuildMissingFinishedFallsBackToEmptyState(t *testing.T) {
	const base = "logs/periodic/2/"
	c := fakeGCS(t, map[string]string{})
	ref := prow.Ref{Bucket: "bkt", ObjectPath: strings.TrimSuffix(base, "/"), Job: "periodic", RunID: "2"}

	d, err := Build(context.Background(), c, ref, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Run.State != "" || d.Run.Result != "" {
		t.Fatalf("expected empty state, got %+v", d.Run)
	}
	if d.Tests != nil || d.Infra != nil {
		t.Fatalf("expected no optional sections: tests=%+v infra=%+v", d.Tests, d.Infra)
	}
	if d.Artifacts == nil || d.Artifacts.Count != 0 {
		t.Fatalf("artifacts: %+v", d.Artifacts)
	}
}

func TestBuildHistoryUsesIndex(t *testing.T) {
	const base = "pr-logs/pull/org_repo/2364/job/2123/"
	const job = "job"
	objects := map[string]string{
		base + "finished.json": `{"result":"SUCCESS"}`,
		// Presubmit run index: one .txt per run pointing at its GCS path.
		"pr-logs/directory/" + job + "/2123.txt":         "gs://bkt/" + strings.TrimSuffix(base, "/"),
		"pr-logs/directory/" + job + "/latest-build.txt": "2123",
		"pr-logs/directory/" + job + "/notanumber.txt":   "gs://bkt/ignored",
	}
	c := fakeGCS(t, objects)
	ref := prow.Ref{Bucket: "bkt", ObjectPath: strings.TrimSuffix(base, "/"), Job: job, RunID: "2123"}

	d, err := Build(context.Background(), c, ref, Options{History: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Job == nil || d.Job.Health == nil {
		t.Fatal("expected health summary")
	}
	if d.Job.Health.Runs != 1 || d.Job.Health.Pass != 1 {
		t.Fatalf("health: %+v", d.Job.Health)
	}
}

// --window reaches the embedded job block. Without it the whole retained
// history is summarised; with it, only the runs inside the window.
func TestBuildHistoryHonoursWindow(t *testing.T) {
	const (
		base = "pr-logs/pull/org_repo/2364/job/2123/"
		job  = "job"
	)
	newest := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	older := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	finished := func(result string, at time.Time) string {
		return fmt.Sprintf(`{"result":%q,"timestamp":%d}`, result, at.Unix())
	}
	objects := map[string]string{
		base + "finished.json": finished("SUCCESS", newest),
		// A second, older run of the same job, reachable only through the index.
		"pr-logs/pull/org_repo/2364/job/2000/finished.json": finished("FAILURE", older),
		"pr-logs/directory/" + job + "/2123.txt":            "gs://bkt/" + strings.TrimSuffix(base, "/"),
		"pr-logs/directory/" + job + "/2000.txt":            "gs://bkt/pr-logs/pull/org_repo/2364/job/2000",
		"pr-logs/directory/" + job + "/latest-build.txt":    "2123",
	}
	ref := prow.Ref{Bucket: "bkt", ObjectPath: strings.TrimSuffix(base, "/"), Job: job, RunID: "2123"}

	all, err := Build(context.Background(), fakeGCS(t, objects), ref, Options{History: true})
	if err != nil {
		t.Fatal(err)
	}
	if all.Job == nil || all.Job.Health == nil {
		t.Fatal("expected a health summary")
	}
	if all.Job.Health.Runs != 2 || all.Job.Health.Fail != 1 {
		t.Fatalf("all history: %+v", all.Job.Health)
	}

	windowed, err := Build(context.Background(), fakeGCS(t, objects), ref,
		Options{History: true, Window: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if windowed.Job == nil || windowed.Job.Health == nil {
		t.Fatal("expected a health summary")
	}
	if windowed.Job.Health.Runs != 1 || windowed.Job.Health.Fail != 0 {
		t.Fatalf("windowed history: %+v", windowed.Job.Health)
	}
	if !windowed.Job.Health.From.Equal(newest.Add(-24 * time.Hour)) {
		t.Errorf("from = %v, want the window's start", windowed.Job.Health.From)
	}
}

var _ = health.VerdictUnknown
