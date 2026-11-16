package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/gcstest"
	"github.com/sebrandon1/whyfail/internal/run"
	"github.com/sebrandon1/whyfail/internal/term"
)

func TestTopSignature(t *testing.T) {
	cases := []struct {
		name string
		d    *run.Digest
		want string
	}{
		{
			name: "blocking test wins",
			d: &run.Digest{Tests: &run.Tests{Failed: []run.TestFailure{
				{Name: "informing"},
				{Name: "blocking", Blocking: true},
			}}},
			want: "blocking",
		},
		{
			name: "first failed test",
			d:    &run.Digest{Tests: &run.Tests{Failed: []run.TestFailure{{Name: "first"}}}},
			want: "first",
		},
		{
			name: "monitor signature",
			d:    &run.Digest{Tests: &run.Tests{Monitors: []run.TestFailure{{Name: "m", Signature: "sig"}}}},
			want: "sig",
		},
		{
			name: "monitor name without signature",
			d:    &run.Digest{Tests: &run.Tests{Monitors: []run.TestFailure{{Name: "m"}}}},
			want: "m",
		},
		{
			name: "failed step fallback",
			d:    &run.Digest{Run: &run.RunInfo{Steps: run.StepsInfo{Failed: []run.StepRef{{Name: "step"}}}}},
			want: "step",
		},
		{
			name: "nothing",
			d:    &run.Digest{},
			want: "",
		},
	}
	for _, c := range cases {
		if got := topSignature(c.d); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"24h", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"nonsense", 0, false},
		{"xd", 0, false},
	}
	for _, c := range cases {
		got, err := parseSince(c.in)
		if c.ok != (err == nil) {
			t.Errorf("%q: err=%v", c.in, err)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestStringField(t *testing.T) {
	m := map[string]any{"user": map[string]any{"login": "alice"}}
	if got := stringField(m, "user", "login"); got != "alice" {
		t.Errorf("got %q", got)
	}
	if got := stringField(m, "missing", "login"); got != "" {
		t.Errorf("missing object: %q", got)
	}
	if got := stringField(m, "user", "sha"); got != "" {
		t.Errorf("missing field: %q", got)
	}
}

// fakeGH answers gh api requests from a fixed endpoint map.
func fakeGH(t *testing.T, pages map[string]string) ghAPI {
	t.Helper()
	return func(endpoint string, paginate bool) ([]byte, error) {
		if !paginate {
			t.Errorf("%s: want a paginated request", endpoint)
		}
		body, ok := pages[endpoint]
		if !ok {
			t.Fatalf("unexpected endpoint %q", endpoint)
		}
		return []byte(body), nil
	}
}

func TestCollectPRChecks(t *testing.T) {
	const (
		sha      = "abc123"
		base     = "repos/org/repo/commits/" + sha
		statuses = base + "/statuses?per_page=100"
		runs     = base + "/check-runs?per_page=100&filter=latest"
	)
	// Two pages of statuses, as `gh api --paginate` emits them: concatenated
	// arrays with no separator.
	statusPages := `[{"context":"ci/prow/e2e-aws","state":"failure"}]` +
		`[{"context":"ci/prow/unit","state":"success"},{"context":"ci/prow/e2e-aws","state":"success"}]`
	runPages := `{"total_count":3,"check_runs":[{"name":"build","status":"completed","conclusion":"failure"},` +
		`{"name":"lint","status":"completed","conclusion":"success"},` +
		`{"name":"e2e","status":"in_progress","conclusion":null}]}`

	cards, err := collectPRChecks(context.Background(), fakeGH(t, map[string]string{
		statuses: statusPages,
		runs:     runPages,
	}), gcstest.Client(t, &gcstest.Dataset{Objects: map[string][]byte{}, Listing: []gcstest.Item{
		{Name: "pr-logs/directory/pull-ci-org-repo-main-e2e-aws/latest-build.txt"},
	}}), gcstest.Bucket, "org", "repo", "main", sha, 0)
	if err != nil {
		t.Fatal(err)
	}

	// e2e-aws failed on the first page and passed on the second; the first
	// failing entry wins. build is a failing check run, lint passed, and e2e is
	// still running so it is not reported.
	want := []struct {
		check string
		prow  bool
	}{{"ci/prow/e2e-aws", true}, {"build", false}}
	if len(cards) != len(want) {
		t.Fatalf("cards = %+v, want %d", cards, len(want))
	}
	for i, w := range want {
		if cards[i].Check != w.check || cards[i].Prow != w.prow {
			t.Errorf("card %d = %+v, want check %q prow %v", i, cards[i], w.check, w.prow)
		}
	}
	if got := cards[0].Job; got != "pull-ci-org-repo-main-e2e-aws" {
		t.Errorf("synthesised job = %q", got)
	}
	if cards[1].Job != "" {
		t.Errorf("non-Prow check got a job name: %q", cards[1].Job)
	}
}

// logDataset builds a small run tree shaped like Prow's real artifact layout.
func logDataset(t *testing.T) (*gcstest.Dataset, string) {
	t.Helper()
	const run = "pr-logs/pull/org_repo/1/job/42"
	files := map[string]string{
		run + "/build-log.txt":                                  "job log",
		run + "/artifacts/release/build-log.txt":                "release log",
		run + "/artifacts/aws-e2e/provision/build-log.txt":      "provision log",
		run + "/artifacts/aws-e2e/gather/build-log.txt":         "gather log",
		run + "/artifacts/aws-e2e/artifacts/deep/build-log.txt": "deep log",
		run + "/artifacts/build-logs/src-amd64.log":             "build log, not a step",
	}
	d := &gcstest.Dataset{Objects: map[string][]byte{}}
	for name, body := range files {
		d.Objects[name] = []byte(body)
		d.Listing = append(d.Listing, gcstest.Item{Name: name})
	}
	// GCS returns a listing in lexical order; the fallback scan depends on it.
	sort.Slice(d.Listing, func(i, j int) bool { return d.Listing[i].Name < d.Listing[j].Name })
	return d, run
}

// countRequests counts the requests recorded exactly as want, so the narrow
// artifacts listing is not mistaken for a full-run listing.
func countRequests(reqs []string, want string) int {
	n := 0
	for _, r := range reqs {
		if r == want {
			n++
		}
	}
	return n
}

func TestResolveStepLog(t *testing.T) {
	cases := []struct {
		name     string
		step     string
		want     string
		wantErr  string
		wantList int // full-run listings; the whole point is that this stays 0
	}{
		{name: "job pseudo-step", step: "job", want: "job log"},
		{name: "direct child of artifacts", step: "release", want: "release log"},
		{name: "under the test directory", step: "provision", want: "provision log"},
		{name: "nested deeper, last-resort scan", step: "deep", want: "deep log", wantList: 1},
		{name: "unknown step", step: "nope", wantErr: `no build-log.txt found for step "nope"`, wantList: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, run := logDataset(t)
			client := gcstest.Client(t, d)

			target, data, err := resolveStepLog(context.Background(), client, gcstest.Bucket, run, c.step)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != c.want {
					t.Errorf("data = %q, want %q", data, c.want)
				}
				if !strings.HasPrefix(target, run+"/") {
					t.Errorf("target = %q, want a path inside the run", target)
				}
			}
			if got := countRequests(d.Requests(), "list "+run+"/"); got != c.wantList {
				t.Errorf("full-run listings = %d, want %d (requests: %v)", got, c.wantList, d.Requests())
			}
		})
	}
}

func TestResolveStepLogJobWithoutLog(t *testing.T) {
	d := &gcstest.Dataset{Objects: map[string][]byte{"other/x": []byte("x")}, Listing: []gcstest.Item{{Name: "other/x"}}}
	_, _, err := resolveStepLog(context.Background(), gcstest.Client(t, d), gcstest.Bucket, "other", "job")
	if err == nil || !strings.Contains(err.Error(), `no build-log.txt found for step "job"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestListArtifacts(t *testing.T) {
	const prefix = "run/artifacts/"
	d := &gcstest.Dataset{Objects: map[string][]byte{}}
	for i := 0; i < 250; i++ {
		name := prefix + fmt.Sprintf("obj-%03d", i)
		d.Objects[name] = []byte("x")
		d.Listing = append(d.Listing, gcstest.Item{Name: name, Size: "1"})
	}
	c := gcstest.Client(t, d)

	// The default asks for one extra so it can tell it is truncating.
	objs, truncated, err := listArtifacts(context.Background(), c, gcstest.Bucket, prefix, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 200 || !truncated {
		t.Fatalf("len=%d truncated=%v", len(objs), truncated)
	}

	// Unlimited reproduces the old whole-listing behaviour.
	objs, truncated, err = listArtifacts(context.Background(), c, gcstest.Bucket, prefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 250 || truncated {
		t.Fatalf("len=%d truncated=%v", len(objs), truncated)
	}

	// A prefix with fewer objects than the limit is not truncated.
	small := &gcstest.Dataset{
		Objects: map[string][]byte{"p/a": []byte("x")},
		Listing: []gcstest.Item{{Name: "p/a", Size: "1"}},
	}
	objs, truncated, err = listArtifacts(context.Background(), gcstest.Client(t, small), gcstest.Bucket, "p/", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || truncated {
		t.Fatalf("small: len=%d truncated=%v", len(objs), truncated)
	}
}

func TestDoctorLocal(t *testing.T) {
	var buf bytes.Buffer
	doctorLocal(&buf, term.Config(new(bytes.Buffer), "never", 100), "/tmp/cache")
	out := buf.String()
	for _, want := range []string{"version:", "cache:     /tmp/cache", "stats:", "binary:", "gh:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "whyfail")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameFile(real, link) {
		t.Error("a symlink to the same file must compare equal")
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if sameFile(real, other) {
		t.Error("different files compared equal")
	}
}
