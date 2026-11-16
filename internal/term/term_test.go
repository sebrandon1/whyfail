package term

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestTrunc(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"hello world", 5, "hell…"},
		{"héllo wörld", 6, "héllo…"},
		{"a\nb", 5, "a b"},
		{"anything", 0, ""},
		{"abc", 1, "…"},
	}
	for _, c := range cases {
		if got := Trunc(c.in, c.n); got != c.want {
			t.Errorf("Trunc(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestTruncMiddle(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short.txt", 20, "short.txt"},
		{"artifacts/e2e-flow/artifacts/junit/junit_e2e.xml", 24, "artifac…junit_e2e.xml"},
		{"artifacts/e2e-flow/artifacts/junit/junit_e2e.xml", 32, "artifacts/…junit/junit_e2e.xml"},
		{"abcdef", 3, "ab…"},
		{"abcdef", 0, ""},
	}
	for _, c := range cases {
		if got := TruncMiddle(c.in, c.n); got != c.want {
			t.Errorf("TruncMiddle(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	if got := len([]rune(TruncMiddle("héllo wörld and more", 8))); got != 8 {
		t.Errorf("rune count = %d, want 8", got)
	}
}

func TestVisibleAndPlainBytes(t *testing.T) {
	s := ansiRed + "abc" + ansiReset + "de"
	if got := Visible(s); got != 5 {
		t.Errorf("Visible = %d, want 5", got)
	}
	if got := PlainBytes(s); got != 5 {
		t.Errorf("PlainBytes = %d, want 5", got)
	}
	if got := Visible("héllo"); got != 5 {
		t.Errorf("Visible(multi-byte) = %d, want 5", got)
	}
	if got := PlainBytes("héllo"); got != 6 {
		t.Errorf("PlainBytes(multi-byte) = %d, want 6", got)
	}
}

func TestPad(t *testing.T) {
	styled := ansiRed + "ab" + ansiReset
	if got := PadRight(styled, 4); Visible(got) != 4 {
		t.Errorf("PadRight visible = %d, want 4 (%q)", Visible(got), got)
	}
	if got := PadLeft(styled, 4); Visible(got) != 4 || !strings.HasPrefix(got, "  ") {
		t.Errorf("PadLeft = %q", got)
	}
	if got := PadRight("abcd", 2); got != "abcd" {
		t.Errorf("PadRight should not shorten, got %q", got)
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("the quick brown fox jumps over the lazy dog", "    ", 30)
	for _, line := range got {
		if Visible(line) > 30 {
			t.Errorf("line too wide (%d): %q", Visible(line), line)
		}
		if !strings.HasPrefix(line, "    ") {
			t.Errorf("missing indent: %q", line)
		}
	}
	if strings.Join(strings.Fields(strings.Join(got, " ")), " ") != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("words lost: %q", got)
	}

	long := Wrap(strings.Repeat("x", 40), "", 20)
	for _, line := range long {
		if Visible(line) > 20 {
			t.Errorf("hard break too wide: %q", line)
		}
	}
	if len(long) != 2 {
		t.Errorf("hard break produced %d lines, want 2", len(long))
	}

	if got := Wrap("a\n\nb", "> ", 40); len(got) != 3 {
		t.Errorf("blank line handling: %q", got)
	}
}

func TestRule(t *testing.T) {
	st := Style{width: 20}
	if got := Visible(st.Rule()); got != 18 {
		t.Errorf("Rule width = %d, want 18", got)
	}
	if got := Visible(Style{width: 0}.Rule()); got != 1 {
		t.Errorf("Rule at width 0 = %d, want 1", got)
	}
}

func TestStylePainting(t *testing.T) {
	plain := Style{}
	if got := plain.Red("x"); got != "x" {
		t.Errorf("plain style should not paint, got %q", got)
	}
	color := Style{color: true}
	if got := color.Red("x"); got != ansiRed+"x"+ansiReset {
		t.Errorf("colored style = %q", got)
	}
	if got := color.Red(""); got != "" {
		t.Errorf("empty string should stay empty, got %q", got)
	}
}

func TestConfigColorModes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// A regular file is not a terminal, so auto is plain text.
	if st := Config(f, "auto", 0); st.Color() {
		t.Error("auto should not colour a non-terminal")
	}
	if st := Config(f, "always", 0); !st.Color() {
		t.Error("always should colour regardless of the writer")
	}
	if st := Config(f, "never", 0); st.Color() {
		t.Error("never must not colour")
	}
}

func TestConfigEnvironment(t *testing.T) {
	if got := Config(new(bytes.Buffer), "auto", 0).Width(); got != DefaultWidth {
		t.Errorf("default width = %d, want %d", got, DefaultWidth)
	}
	t.Setenv("COLUMNS", "132")
	if got := Config(new(bytes.Buffer), "auto", 0).Width(); got != 132 {
		t.Errorf("COLUMNS width = %d, want 132", got)
	}
	if got := Config(new(bytes.Buffer), "auto", 90).Width(); got != 90 {
		t.Errorf("--width should win, got %d", got)
	}
	if got := Config(new(bytes.Buffer), "auto", 5).Width(); got != 20 {
		t.Errorf("width should clamp to 20, got %d", got)
	}

	t.Setenv("NO_COLOR", "1")
	if envAllowsColor() {
		t.Error("NO_COLOR should disable colour")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if envAllowsColor() {
		t.Error("TERM=dumb should disable colour")
	}
}
