package prow

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		in     string
		bucket string
		path   string
		job    string
		run    string
		ok     bool
	}{
		{
			in:     "https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/org_repo/1/job/2123",
			bucket: "test-platform-results-public",
			path:   "pr-logs/pull/org_repo/1/job/2123",
			job:    "job", run: "2123", ok: true,
		},
		{
			in:     "https://prow.ci.openshift.org/view/gcs/b/p/1",
			bucket: "b", path: "p/1", job: "p", run: "1", ok: true,
		},
		{
			in:     "gs://bucket/logs/periodic-job/99",
			bucket: "bucket", path: "logs/periodic-job/99", job: "periodic-job", run: "99", ok: true,
		},
		{in: "https://github.com/openshift/release/pull/1", ok: false},
		{in: "gs://onlybucket", ok: false},
		{in: "", ok: false},
		{in: "https://prow.ci.openshift.org/view/other/x", ok: false},
		{in: "https://storage.googleapis.com/storage/v1/b/x/o?prefix=y", ok: false},
		{
			in:     "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/b/p/1",
			bucket: "b", path: "p/1", job: "p", run: "1", ok: true,
		},
	}
	for _, c := range cases {
		ref, err := ParseRef(c.in)
		if c.ok != (err == nil) {
			t.Fatalf("%q: err=%v", c.in, err)
		}
		if !c.ok {
			continue
		}
		if ref.Bucket != c.bucket || ref.ObjectPath != c.path || ref.Job != c.job || ref.RunID != c.run {
			t.Errorf("%q: got %+v", c.in, ref)
		}
	}
}

func TestParseArtifactURL(t *testing.T) {
	cases := []struct {
		in     string
		bucket string
		object string
		ok     bool
	}{
		{"https://storage.googleapis.com/test-platform-results-public/pr-logs/x/1/build-log.txt", "test-platform-results-public", "pr-logs/x/1/build-log.txt", true},
		{"gs://bucket/a/b.txt", "bucket", "a/b.txt", true},
		{"https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/b/o/x.txt", "b", "o/x.txt", true},
		{"https://storage.googleapis.com/onlybucket", "", "", false},
		{"https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/other", "", "", false},
		{"gs://onlybucket", "", "", false},
		{"https://example.com/x", "", "", false},
	}
	for _, c := range cases {
		b, o, err := ParseArtifactURL(c.in)
		if c.ok != (err == nil) {
			t.Errorf("%q: err=%v", c.in, err)
			continue
		}
		if c.ok && (b != c.bucket || o != c.object) {
			t.Errorf("%q: got %q %q", c.in, b, o)
		}
	}
}

func TestIsRunURL(t *testing.T) {
	cases := map[string]bool{
		"https://prow.ci.openshift.org/view/gs/b/pr-logs/pull/o_r/1/job/2123": true,
		"gs://b/logs/periodic/99":                     true,
		"https://github.com/openshift/release/pull/1": false,
		"": false,
	}
	for in, want := range cases {
		if got := IsRunURL(in); got != want {
			t.Errorf("IsRunURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestObjectAndViewURL(t *testing.T) {
	ref := Ref{Bucket: "b", ObjectPath: "pr-logs/pull/o_r/1/job/2123"}
	view := ViewURL(ref)
	if view != "https://prow.ci.openshift.org/view/gs/b/pr-logs/pull/o_r/1/job/2123" {
		t.Fatalf("ViewURL: %q", view)
	}
	back, err := ParseRef(view)
	if err != nil {
		t.Fatal(err)
	}
	if back.Bucket != ref.Bucket || back.ObjectPath != ref.ObjectPath {
		t.Fatalf("round trip: %+v", back)
	}
	if back.Job != "job" || back.RunID != "2123" {
		t.Fatalf("round trip job/run: %+v", back)
	}

	if got := ObjectURL("b", "p/x y.txt"); got != "https://storage.googleapis.com/b/p/x%20y.txt" {
		t.Fatalf("ObjectURL: %q", got)
	}
}
