// Package term provides terminal styling, width detection and text helpers for
// whyfail's human-readable output. It has no dependencies.
package term

import (
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Style carries the presentation decisions for one rendering pass. The zero
// Style is plain text at the default width.
type Style struct {
	color bool
	width int
}

// DefaultWidth is used when the terminal width cannot be determined.
const DefaultWidth = 100

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

// Detect reports the style implied by the environment for writer w.
func Detect(w io.Writer) Style {
	return Style{color: isTerminal(w) && envAllowsColor(), width: envWidth()}
}

// Config resolves a Style for w. mode is "auto", "always" or "never"; width <= 0
// means detect it. Either setting overrides what Detect found.
func Config(w io.Writer, mode string, width int) Style {
	st := Detect(w)
	switch mode {
	case "always":
		st.color = true
	case "never":
		st.color = false
	}
	if width > 0 {
		st.width = width
	}
	if st.width < 20 {
		st.width = 20
	}
	return st
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func envAllowsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR") == "0" {
		return false
	}
	return os.Getenv("TERM") != "dumb"
}

func envWidth() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS"))); err == nil && n > 0 {
		return n
	}
	return DefaultWidth
}

// Color reports whether escapes will be emitted.
func (s Style) Color() bool { return s.color }

// Width is the number of columns the output should fit in.
func (s Style) Width() int { return s.width }

func (s Style) paint(code, v string) string {
	if !s.color || v == "" {
		return v
	}
	return code + v + ansiReset
}

// Bold renders v bold.
func (s Style) Bold(v string) string { return s.paint(ansiBold, v) }

// Dim renders v faint.
func (s Style) Dim(v string) string { return s.paint(ansiDim, v) }

// Red renders v in red.
func (s Style) Red(v string) string { return s.paint(ansiRed, v) }

// Green renders v in green.
func (s Style) Green(v string) string { return s.paint(ansiGreen, v) }

// Yellow renders v in yellow.
func (s Style) Yellow(v string) string { return s.paint(ansiYellow, v) }

// Blue renders v in blue.
func (s Style) Blue(v string) string { return s.paint(ansiBlue, v) }

// Cyan renders v in cyan.
func (s Style) Cyan(v string) string { return s.paint(ansiCyan, v) }

// Rule returns a horizontal rule that fits the width.
func (s Style) Rule() string {
	n := s.width - 2
	if n < 1 {
		n = 1
	}
	return s.Dim(strings.Repeat("─", n))
}

// PlainBytes reports the length of s in bytes, ignoring ANSI SGR sequences. It
// is used for the output accounting in `whyfail gain`.
func PlainBytes(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i = sgrEnd(s, i)
			continue
		}
		i++
		n++
	}
	return n
}

// Visible reports the width of s in characters, ignoring ANSI SGR sequences.
func Visible(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i = sgrEnd(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// sgrEnd returns the index just past the escape sequence starting at i.
func sgrEnd(s string, i int) int {
	j := i + 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j < len(s) {
		j++
	}
	return j
}

// Trunc shortens s to at most n characters, marking the cut with an ellipsis.
// Slicing is rune-aware so multi-byte characters are never split.
func Trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// TruncMiddle shortens s to at most n characters by eliding from the middle, so
// both the leading path and the trailing component survive.
func TruncMiddle(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return Trunc(s, n)
	}
	head := (n - 1) / 3
	tail := n - 1 - head
	start := len(r) - tail
	// Prefer to resume at a path separator so the elided tail keeps whole
	// components: "artifacts/…/junit_e2e.xml" rather than "artifac…it/junit_e2e.xml".
	for start < len(r) && r[start-1] != '/' {
		start++
	}
	if start >= len(r) {
		start = len(r) - tail
	}
	return string(r[:head]) + "…" + string(r[start:])
}

// PadRight pads s with spaces to n visible characters.
func PadRight(s string, n int) string {
	if pad := n - Visible(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// PadLeft left-pads s with spaces to n visible characters.
func PadLeft(s string, n int) string {
	if pad := n - Visible(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// WrapItems folds items onto lines joined by sep, never splitting an item that
// fits on a line. Items longer than a line fall back to Wrap. Every returned
// line starts with indent.
func WrapItems(items []string, indent, sep string, width int) []string {
	avail := width - Visible(indent)
	if avail < 12 {
		avail = 12
	}
	var out []string
	line := ""
	flush := func() {
		if line != "" {
			out = append(out, indent+line)
			line = ""
		}
	}
	for _, it := range items {
		if Visible(it) > avail {
			flush()
			out = append(out, Wrap(it, indent, width)...)
			continue
		}
		switch {
		case line == "":
			line = it
		case Visible(line)+Visible(sep)+Visible(it) <= avail:
			line += sep + it
		default:
			out = append(out, indent+strings.TrimRight(line, " "+strings.TrimSpace(sep)))
			line = it
		}
	}
	flush()
	return out
}

// Wrap folds s onto lines that fit width, breaking on spaces where it can and
// hard-breaking tokens longer than a line. Every returned line starts with
// indent.
func Wrap(s, indent string, width int) []string {
	avail := width - Visible(indent)
	if avail < 12 {
		avail = 12
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, indent)
			continue
		}
		line := ""
		for _, w := range words {
			for Visible(w) > avail {
				if line != "" {
					out = append(out, indent+line)
					line = ""
				}
				r := []rune(w)
				out = append(out, indent+string(r[:avail]))
				w = string(r[avail:])
			}
			switch {
			case line == "":
				line = w
			case Visible(line)+1+Visible(w) <= avail:
				line += " " + w
			default:
				out = append(out, indent+line)
				line = w
			}
		}
		if line != "" {
			out = append(out, indent+line)
		}
	}
	return out
}

// Plain returns s with SGR escape sequences removed. It exists so a caller can
// fold text without risking a split escape sequence.
func Plain(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := sgrEnd(s, i)
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
