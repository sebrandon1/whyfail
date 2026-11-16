package main

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/gcstest"
	"github.com/sebrandon1/whyfail/internal/health"
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

func TestIsText(t *testing.T) {
	if !isText([]byte("hello\nworld")) {
		t.Error("plain text should be text")
	}
	if isText([]byte{'a', 0, 'b'}) {
		t.Error("NUL byte should be binary")
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

func TestPRCardLine(t *testing.T) {
	st := term.Config(new(bytes.Buffer), "never", 100)
	const job = "pull-ci-org-repo-main-e2e-aws"

	// A job that is not in the bucket must not panic or read as healthy.
	unknown := prCardLine(st, prCard{Job: job}, 40)
	if !strings.Contains(unknown, "UNKNOWN") || !strings.Contains(unknown, "no history in this bucket") {
		t.Errorf("nil health: %q", unknown)
	}

	flaky := prCardLine(st, prCard{Job: job, Health: &health.Summary{
		Verdict: health.VerdictFlaky, PassRate: 0.5, Runs: 10,
	}}, 40)
	for _, want := range []string{"FLAKY", "50.0%", "10 runs"} {
		if !strings.Contains(flaky, want) {
			t.Errorf("flaky line missing %q: %q", want, flaky)
		}
	}

	// The job column is 40; the suffix must still fit the 100-column style.
	long := prCardLine(st, prCard{Job: strings.Repeat("x", 200)}, 40)
	if n := term.Visible(strings.SplitN(long, "\n", 2)[0]); n > st.Width() {
		t.Errorf("line is %d columns wide: %q", n, long)
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
