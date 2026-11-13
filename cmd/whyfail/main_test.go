package main

import (
	"testing"
	"time"

	"github.com/sebrandon1/whyfail/internal/run"
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
