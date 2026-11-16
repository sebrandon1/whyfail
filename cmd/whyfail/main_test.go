package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

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
