// Package render formats whyfail output for humans.
package render

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/run"
	"github.com/sebrandon1/whyfail/internal/stats"
	"github.com/sebrandon1/whyfail/internal/term"
)

// gutter is the width of the label column; "ARTIFACTS" is the widest label.
const gutter = 9

// rowIndent aligns continuation lines under the first value column.
var rowIndent = strings.Repeat(" ", 2+gutter+1)

// VerdictLabel returns an icon and short label for a health verdict.
func VerdictLabel(v health.Verdict) (string, string) {
	switch v {
	case health.VerdictBroken:
		return "⛔", "BROKEN"
	case health.VerdictFlaky:
		return "⚠", "FLAKY"
	case health.VerdictDegraded:
		return "~", "DEGRADED"
	case health.VerdictHealthy:
		return "✓", "HEALTHY"
	default:
		return "?", "UNKNOWN"
	}
}

// verdictStyle paints s according to the verdict.
func verdictStyle(st term.Style, v health.Verdict, s string) string {
	switch v {
	case health.VerdictBroken:
		return st.Red(s)
	case health.VerdictFlaky:
		return st.Yellow(s)
	case health.VerdictHealthy:
		return st.Green(s)
	case health.VerdictDegraded:
		return st.Blue(s)
	default:
		return st.Dim(s)
	}
}

// VerdictText paints s with the colour that belongs to verdict v.
func VerdictText(st term.Style, v health.Verdict, s string) string {
	return verdictStyle(st, v, s)
}

func resultStyle(st term.Style, result string) string {
	switch strings.ToUpper(result) {
	case "SUCCESS":
		return st.Green(strings.ToUpper(result))
	case "FAILURE":
		return st.Red(strings.ToUpper(result))
	case "ABORTED":
		return st.Yellow(strings.ToUpper(result))
	default:
		return strings.ToUpper(result)
	}
}

// row writes one label/value pair.
func row(w io.Writer, st term.Style, name, value string) {
	fmt.Fprintf(w, "  %s %s\n", st.Dim(term.PadRight(name, gutter)), value)
}

// rowStyled writes a labelled row, folding the value to the terminal width and
// painting every resulting line.
func rowStyled(w io.Writer, st term.Style, name, value string, paint func(string) string) {
	if term.Visible(value) <= st.Width()-len(rowIndent) && !strings.Contains(value, "\n") {
		fmt.Fprintf(w, "  %s %s\n", st.Dim(term.PadRight(name, gutter)), paint(value))
		return
	}
	lines := term.Wrap(value, rowIndent, st.Width())
	fmt.Fprintf(w, "  %s %s\n", st.Dim(term.PadRight(name, gutter)), paint(strings.TrimPrefix(lines[0], rowIndent)))
	for _, l := range lines[1:] {
		fmt.Fprintln(w, paint(l))
	}
}

// fit truncates s so it fits on a labelled row.
func fit(st term.Style, s string) string {
	return term.Trunc(s, st.Width()-len(rowIndent))
}

// savingsGutter is the label column the savings card uses; "next milestone" is
// the widest label it prints.
const savingsGutter = 21

// grow writes a labelled row with the wider gutter the savings card needs. On a
// narrow terminal the value folds under itself; styling is dropped first,
// because a wrapped escape sequence would corrupt the line.
func grow(w io.Writer, st term.Style, name, value string) {
	labelW := term.Visible(name)
	if labelW < savingsGutter {
		labelW = savingsGutter
	}
	// A long group label must not push the value off the line.
	if limit := st.Width() / 2; labelW > limit {
		name = term.TruncMiddle(name, limit)
		labelW = term.Visible(name)
	}
	indent := strings.Repeat(" ", 2+labelW+1)
	if term.Visible(value)+len(indent) <= st.Width() && !strings.Contains(value, "\n") {
		fmt.Fprintf(w, "  %s %s\n", st.Dim(term.PadRight(name, labelW)), value)
		return
	}
	lines := term.Wrap(term.Plain(value), indent, st.Width())
	fmt.Fprintf(w, "  %s %s\n", st.Dim(term.PadRight(name, labelW)), strings.TrimPrefix(lines[0], indent))
	for _, l := range lines[1:] {
		fmt.Fprintln(w, l)
	}
}

// plural returns one when n is exactly one, otherwise many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func stepNames(refs []run.StepRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name
	}
	return out
}

func testNames(fs []run.TestFailure) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

// excerptLines caps how much of a test error is shown; the whole thing is in
// --json and in the artifact log.
const excerptLines = 2

// writeExcerpt prints the first few lines of an error, folded to the width.
func writeExcerpt(w io.Writer, st term.Style, err, indent string) {
	if err == "" {
		return
	}
	lines := term.Wrap(err, indent, st.Width())
	more := 0
	if len(lines) > excerptLines {
		more = len(lines) - excerptLines
		lines = lines[:excerptLines]
	}
	for _, l := range lines {
		fmt.Fprintln(w, st.Dim(l))
	}
	if more > 0 {
		fmt.Fprintf(w, "%s%s\n", indent, st.Dim(fmt.Sprintf("… (%d more %s in --json)",
			more, plural(more, "line", "lines"))))
	}
}

// nameWidth is the column width that fits the longest name without pushing the
// rest of the row past the terminal width.
func nameWidth(st term.Style, names []string, reserved int) int {
	widest := 0
	for _, n := range names {
		if v := term.Visible(n); v > widest {
			widest = v
		}
	}
	if limit := st.Width() - reserved; widest > limit {
		widest = limit
	}
	if widest < 12 {
		widest = 12
	}
	return widest
}

// verdictLine writes the icon, verdict and a dim, width-folded summary.
func verdictLine(w io.Writer, st term.Style, v health.Verdict, blurb string) {
	icon, label := VerdictLabel(v)
	const hang = 12 // icon + space + padded label + two spaces
	indent := strings.Repeat(" ", hang)
	lines := term.Wrap(blurb, indent, st.Width())
	fmt.Fprintf(w, "%s %s  %s\n",
		verdictStyle(st, v, icon),
		verdictStyle(st, v, term.PadRight(label, 8)),
		st.Dim(strings.TrimPrefix(lines[0], indent)))
	for _, l := range lines[1:] {
		fmt.Fprintln(w, st.Dim(l))
	}
}

// Digest writes a human-readable digest.
func Digest(w io.Writer, d *run.Digest, st term.Style) {
	r := d.Run
	fmt.Fprintln(w, st.Bold(r.JobName))
	if d.Job != nil && d.Job.Health != nil {
		h := d.Job.Health
		var parts []string
		if h.Pass+h.Fail > 0 {
			parts = append(parts, health.FormatRate(h.PassRate)+" pass")
		}
		if h.Runs > 0 {
			parts = append(parts, fmt.Sprintf("%d runs since %s", h.Runs, h.From.Format("2006-01-02")))
		}
		if h.LastPass != nil {
			parts = append(parts, "last pass "+h.LastPass.Format("2006-01-02"))
		}
		verdictLine(w, st, h.Verdict, strings.Join(parts, " · "))
	}
	fmt.Fprintln(w)

	writeRunRows(w, st, r)
	writeSteps(w, st, r.Steps)
	writeTests(w, st, d.Tests)
	writeInfra(w, st, d.Infra)
	writeArtifacts(w, st, d.Artifacts)
	writeSignals(w, st, d.Signals)

	fmt.Fprintln(w)
	if d.NextAction != "" {
		row(w, st, "NEXT", d.NextAction)
	}
	fmt.Fprintf(w, "  %s %s\n", st.Dim(term.PadRight("URL", gutter)), st.Dim(r.URL))
}

func writeRunRows(w io.Writer, st term.Style, r *run.RunInfo) {
	var runParts []string
	runParts = append(runParts, r.ID)
	if r.Result != "" {
		runParts = append(runParts, resultStyle(st, r.Result))
	}
	if r.Timing.DurationS > 0 {
		p := FormatDuration(r.Timing.DurationS)
		if r.Timing.TimeoutS > 0 {
			p += fmt.Sprintf("   %.0f%% of %s timeout", r.Timing.PctOfTimeout, FormatDuration(r.Timing.TimeoutS))
		}
		runParts = append(runParts, p)
	}
	row(w, st, "RUN", strings.Join(runParts, "   "))

	if pr := r.PR; pr != nil && pr.Number != 0 {
		v := st.Cyan(fmt.Sprintf("%s/%s#%d", pr.Org, pr.Repo, pr.Number))
		if pr.Author != "" {
			v += "   " + pr.Author
		}
		row(w, st, "PR", v)
		if pr.Title != "" {
			for _, l := range term.Wrap(`"`+pr.Title+`"`, rowIndent, st.Width()) {
				fmt.Fprintln(w, st.Dim(l))
			}
		}
	}

	var meta []string
	if r.Cluster != "" {
		meta = append(meta, r.Cluster)
	}
	if r.Namespace != "" {
		meta = append(meta, r.Namespace)
	}
	if r.RerunCommand != "" {
		meta = append(meta, r.RerunCommand)
	}
	if len(meta) > 0 {
		row(w, st, "CLUSTER", fit(st, strings.Join(meta, " · ")))
	}

	// The reason is the ci-operator's own verdict on why the run failed; show it
	// before the component findings so it is never below the fold.
	if r.Reason != "" {
		row(w, st, "REASON", st.Red(fit(st, r.Reason)))
	}
}

func writeSteps(w io.Writer, st term.Style, s run.StepsInfo) {
	if s.Total == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+st.Rule())
	failed := st.Dim("0 failed")
	if n := len(s.Failed); n > 0 {
		failed = st.Red(fmt.Sprintf("%d failed", n))
	}
	row(w, st, "STEPS", fmt.Sprintf("%d total · %s", s.Total, failed))

	nameW := nameWidth(st, stepNames(s.Failed), 5+1+8)
	for _, f := range s.Failed {
		fmt.Fprintf(w, "  %s  %s %s\n",
			st.Red("✗"),
			term.PadRight(term.TruncMiddle(f.Name, nameW), nameW),
			st.Dim(term.PadLeft(FormatDuration(f.DurationS), 8)))
		writeExcerpt(w, st, f.Error, "       ")
	}

	if len(s.Longest) > 0 {
		parts := make([]string, 0, len(s.Longest))
		for _, l := range s.Longest {
			parts = append(parts, l.Name+" "+FormatDuration(l.DurationS))
		}
		lines := term.WrapItems(parts, rowIndent, " · ", st.Width())
		fmt.Fprintf(w, "  %s %s\n", st.Dim(term.PadRight("LONGEST", gutter)),
			st.Dim(strings.TrimPrefix(lines[0], rowIndent)))
		for _, l := range lines[1:] {
			fmt.Fprintln(w, st.Dim(l))
		}
	}
}

func writeTests(w io.Writer, st term.Style, t *run.Tests) {
	if t == nil {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+st.Rule())

	v := fmt.Sprintf("%d pass · %d fail · %d skip", t.Counts.Passed, t.Counts.Failed, t.Counts.Skipped)
	if t.Counts.Flaked > 0 {
		v += fmt.Sprintf(" · %d flaked", t.Counts.Flaked)
	}
	if t.Source != "" {
		v += "   [" + t.Source + "]"
	}
	rowStyled(w, st, "TESTS", v, func(s string) string { return s })

	nameW := nameWidth(st, testNames(t.Failed), 9)
	for _, f := range t.Failed {
		label, paint := "inform", st.Dim
		if f.Blocking {
			label, paint = "BLOCK", st.Red
		}
		fmt.Fprintf(w, "  %s %s\n", paint(term.PadRight(label, 6)), term.Trunc(f.Name, nameW))
		writeExcerpt(w, st, f.Error, "         ")
	}

	if len(t.Monitors) > 0 {
		groups := t.Causes
		total := t.MonitorTotal
		if len(groups) == 0 {
			groups = run.GroupCauses(t.Monitors)
		}
		if total == 0 {
			total = len(t.Monitors)
		}
		row(w, st, "MONITORS", fmt.Sprintf("%d %s · %d %s", total, plural(total, "failure", "failures"), len(groups), plural(len(groups), "cause", "causes")))
		for i, g := range groups {
			if i >= 6 {
				fmt.Fprintf(w, "  %s\n", st.Dim(fmt.Sprintf("… and %d more causes", len(groups)-6)))
				break
			}
			fmt.Fprintf(w, "  %s  %s\n",
				st.Dim(fmt.Sprintf("%3d×", g.Count)),
				term.TruncMiddle(g.Signature, st.Width()-8))
		}
	}
}

func writeInfra(w io.Writer, st term.Style, in *run.Infra) {
	if in == nil {
		return
	}
	var parts []string
	if in.ClusterProfile != "" {
		parts = append(parts, "profile="+in.ClusterProfile)
	}
	if in.Region != "" {
		parts = append(parts, "region="+in.Region)
	}
	if in.Lease != nil {
		if in.Lease.Slice != "" {
			parts = append(parts, "slice="+in.Lease.Slice)
		}
		parts = append(parts, fmt.Sprintf("avail=%d", in.Lease.AvailableAtAcquire))
		if in.Lease.AcquireS > 0 {
			parts = append(parts, fmt.Sprintf("acquire=%.1fs", in.Lease.AcquireS))
		}
	}
	if in.MachinesTotal > 0 {
		parts = append(parts, fmt.Sprintf("machines=%d", in.MachinesTotal))
	}
	if in.Variant != "" {
		parts = append(parts, "variant="+in.Variant)
	}
	if in.InstallFailed {
		parts = append(parts, "INSTALL FAILED")
	}
	if len(parts) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  "+st.Rule())
		paint := st.Dim
		if in.InstallFailed {
			paint = st.Red
		}
		rowStyled(w, st, "INFRA", strings.Join(parts, " · "), paint)
		if in.Execution != nil {
			row(w, st, "", st.Dim(fmt.Sprintf("ci-operator success=%v · %d %s · %s",
				in.Execution.Success, in.Execution.ErrorCount,
				plural(in.Execution.ErrorCount, "error", "errors"),
				FormatDuration(in.Execution.DurationS))))
		}
	}
}

func writeArtifacts(w io.Writer, st term.Style, a *run.Artifacts) {
	if a == nil || a.Count == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+st.Rule())

	names := notableNames(a.Notable)
	prefix := commonDir(names)
	v := fmt.Sprintf("%d objects · %s", a.Count, FormatBytes(a.TotalBytes))
	if prefix != "" {
		v += "   " + st.Dim("(prefix "+term.TruncMiddle(prefix+"/", 46)+")")
	}
	row(w, st, "ARTIFACTS", v)

	const maxShown = 5
	shown := a.Notable
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	rel := make([]string, len(shown))
	maxName, maxWhy := 0, 0
	for i, n := range shown {
		rel[i] = notableName(n)
		if prefix != "" {
			rel[i] = strings.TrimPrefix(rel[i], prefix+"/")
		}
		if w := term.Visible(rel[i]); w > maxName {
			maxName = w
		}
		if w := term.Visible(n.Why); w > maxWhy {
			maxWhy = w
		}
	}
	budget := st.Width() - (2 + 9 + 2 + 1)
	if budget < 20 {
		budget = 20
	}
	whyW := maxWhy
	if whyW > budget/2 {
		whyW = budget / 2
	}
	nameW := budget - whyW
	if nameW > maxName {
		nameW = maxName
	}
	for i, n := range shown {
		fmt.Fprintf(w, "  %s  %s %s\n",
			st.Dim(term.PadLeft(FormatBytes(n.Bytes), 9)),
			term.PadRight(term.TruncMiddle(rel[i], nameW), nameW),
			st.Dim(term.TruncMiddle(n.Why, whyW)))
	}
	if rest := len(a.Notable) - len(shown); rest > 0 {
		fmt.Fprintf(w, "  %s\n", st.Dim(fmt.Sprintf("… and %d more notable · --json for the full inventory", rest)))
	}
}

func writeSignals(w io.Writer, st term.Style, s *run.Signals) {
	if s == nil {
		return
	}
	var parts []string
	if s.Sippy != "" {
		parts = append(parts, "sippy="+s.Sippy)
	}
	if c := s.SearchCI; c != nil {
		p := fmt.Sprintf("search.ci=%d matches in 48h", c.Matches48h)
		if c.KnownBug != "" {
			p += " (" + c.KnownBug + ")"
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return
	}
	fmt.Fprintln(w)
	rowStyled(w, st, "SIGNALS", strings.Join(parts, " · "), st.Dim)
}

func notableName(n run.Notable) string {
	if n.Short != "" {
		return n.Short
	}
	return n.Name
}

func notableNames(ns []run.Notable) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = notableName(n)
	}
	return out
}

// commonDir returns the deepest directory shared by every name, or "".
func commonDir(names []string) string {
	if len(names) < 2 {
		return ""
	}
	prefix := path.Dir(names[0])
	for _, n := range names {
		for prefix != "" && !strings.HasPrefix(n, prefix+"/") {
			prefix = path.Dir(prefix)
			if prefix == "." || prefix == "/" {
				prefix = ""
			}
		}
	}
	if prefix == "." {
		return ""
	}
	return prefix
}

// Failures writes only the failed steps and tests of a run.
func Failures(w io.Writer, st term.Style, jobName, result string, steps []run.StepRef, tests *run.Tests) {
	fmt.Fprintln(w, st.Bold(jobName), resultStyle(st, result))
	if len(steps) == 0 && (tests == nil || (len(tests.Failed) == 0 && len(tests.Monitors) == 0)) {
		fmt.Fprintln(w)
		row(w, st, "STEPS", st.Green("none failed"))
		return
	}
	fmt.Fprintln(w)

	if len(steps) > 0 {
		row(w, st, "STEPS", st.Red(fmt.Sprintf("%d failed", len(steps))))
		nameW := nameWidth(st, stepNames(steps), 5+1+8)
		for _, s := range steps {
			fmt.Fprintf(w, "  %s  %s %s\n",
				st.Red("✗"),
				term.PadRight(term.TruncMiddle(s.Name, nameW), nameW),
				st.Dim(term.PadLeft(FormatDuration(s.DurationS), 8)))
		}
		fmt.Fprintln(w)
	}
	if tests == nil {
		return
	}
	for _, f := range tests.Failed {
		label, paint := "inform", st.Dim
		if f.Blocking {
			label, paint = "BLOCK", st.Red
		}
		fmt.Fprintf(w, "  %s %s\n", paint(term.PadRight(label, 6)), term.Trunc(f.Name, st.Width()-9))
		writeExcerpt(w, st, f.Error, "         ")
	}
	if len(tests.Monitors) > 0 {
		row(w, st, "MONITORS", fmt.Sprintf("%d failed", len(tests.Monitors)))
		for _, f := range tests.Monitors {
			fmt.Fprintf(w, "  %s  %s\n", st.Dim("monitor"), term.TruncMiddle(f.Name, st.Width()-10))
		}
	}
}

// Job writes a job health card.
func Job(w io.Writer, s *health.Summary, st term.Style) {
	fmt.Fprintln(w, st.Bold(s.Job))
	if s.Runs == 0 {
		row(w, st, "WINDOW", st.Dim("no runs found in the Prow index (job may be renamed or never have run)"))
		return
	}

	verdictLine(w, st, s.Verdict, fmt.Sprintf("%s pass · %d runs · %s → %s",
		health.FormatRate(s.PassRate), s.Runs,
		s.From.Format("2006-01-02"), s.To.Format("2006-01-02")))
	fmt.Fprintln(w)

	row(w, st, "RUNS", fmt.Sprintf("%d total · %d pass · %d fail · %d abort · %d missing",
		s.Runs, s.Pass, s.Fail, s.Abort, s.Missing))
	rowStyled(w, st, "RATE", fmt.Sprintf("%s · recent %s · streak %d",
		health.FormatRate(s.PassRate), health.FormatRate(s.RecentPassRate), s.Streak),
		func(v string) string { return v })
	if s.LastPass != nil {
		row(w, st, "LAST PASS", s.LastPass.Format("2006-01-02"))
	} else {
		row(w, st, "LAST PASS", st.Dim("none in window"))
	}
	if s.DistinctPRs > 0 {
		row(w, st, "FLAKY PRS", fmt.Sprintf("%d distinct %s failing", s.DistinctPRs, plural(s.DistinctPRs, "PR is", "PRs are")))
	}

	var advice string
	switch s.Verdict {
	case health.VerdictBroken:
		advice = "retesting will not help; this job is red for everyone"
	case health.VerdictFlaky:
		advice = "a retest is reasonable"
	case health.VerdictHealthy:
		advice = "this job passes reliably"
	}
	if advice != "" {
		fmt.Fprintln(w)
		row(w, st, "NEXT", verdictStyle(st, s.Verdict, advice))
	}
}

// Gain writes the savings summary.
func Gain(w io.Writer, a stats.Aggregate, bytesPerToken int, baseline time.Duration, st term.Style) {
	if a.Totals.Calls == 0 {
		fmt.Fprintln(w, "no recorded calls yet")
		return
	}
	savedBytes := a.Totals.ReadBytes - a.Totals.EmittedBytes
	if savedBytes < 0 {
		savedBytes = 0
	}
	fmt.Fprintf(w, "%s ~%s across %d calls", st.Bold("whyfail saved"), st.Bold(FormatBytes(savedBytes)), a.Totals.Calls)
	if !a.Since.IsZero() {
		fmt.Fprintf(w, " (since %s)", a.Since.Format("2006-01-02"))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	pct := 0.0
	if a.Totals.ReadBytes > 0 {
		pct = float64(a.Totals.EmittedBytes) / float64(a.Totals.ReadBytes) * 100
	}
	grow(w, st, "calls", fmt.Sprintf("%d   ·  cache hits %d", a.Totals.Calls, a.Totals.CacheHits))
	grow(w, st, "read", fmt.Sprintf("%s → %s emitted (%.1f%% of input)",
		FormatBytes(a.Totals.ReadBytes), FormatBytes(a.Totals.EmittedBytes), pct))
	if a.Totals.ReadBytes > 0 {
		// read includes cache hits; network is what actually came over the wire,
		// so this is the line that shows what the cache is buying.
		cached := a.Totals.ReadBytes - a.Totals.NetworkBytes
		if cached < 0 {
			cached = 0
		}
		grow(w, st, "network", fmt.Sprintf("%s downloaded · %s from cache (%.0f%%)",
			FormatBytes(a.Totals.NetworkBytes), FormatBytes(cached),
			float64(cached)/float64(a.Totals.ReadBytes)*100))
	}
	if a.Totals.InventoryBytes > 0 {
		grow(w, st, "full trees", fmt.Sprintf("%s   %s", FormatBytes(a.Totals.InventoryBytes),
			st.Dim("(upper bound, not all of it would be read)")))
	}
	// Show the division, so the modelled figure carries its own arithmetic and
	// --bytes-per-token is discoverable from the output.
	divisor := int64(max(bytesPerToken, 1))
	tokens := savedBytes / divisor
	grow(w, st, "~tokens saved", fmt.Sprintf("%s   %s",
		st.Yellow(FormatCount(tokens)),
		st.Dim(fmt.Sprintf("(%s ÷ %d bytes/token)", FormatBytes(savedBytes), divisor))))
	if a.Totals.Missing > 0 {
		grow(w, st, "missing", fmt.Sprintf("%s   %s", FormatCount(a.Totals.Missing),
			st.Dim("lookups returned 404")))
	}

	if dl := downloadSeconds(a); dl > 0 {
		grow(w, st, "~download time saved", fmt.Sprintf("%s   %s", FormatDuration(dl), st.Dim("(measured throughput)")))
	}
	if baseline > 0 {
		manual := time.Duration(a.Totals.Calls) * baseline
		tool := time.Duration(a.Totals.WallMS) * time.Millisecond
		if manual > tool {
			grow(w, st, "~manual time saved", fmt.Sprintf("%s   %s",
				FormatDuration((manual-tool).Seconds()),
				st.Dim("(vs "+baseline.String()+" baseline per call; modelled)")))
		}
	}
	if len(a.Verdicts) > 0 {
		grow(w, st, "verdicts", verdictCounts(st, a.Verdicts))
	}
	// Only worth showing once there is visible progress; "0% there" is noise.
	if next := nextMilestone(savedBytes); next > 0 && savedBytes >= next/100 {
		grow(w, st, "next milestone", fmt.Sprintf("%s · %.0f%% there",
			FormatBytes(next), float64(savedBytes)/float64(next)*100))
	}
	fmt.Fprintln(w)
	for _, l := range term.Wrap("(measured numbers come from recorded calls; ~ marks modelled estimates)", "", st.Width()) {
		fmt.Fprintln(w, st.Dim(l))
	}

	if len(a.Groups) > 0 {
		keys := make([]string, 0, len(a.Groups))
		for k := range a.Groups {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return a.Groups[keys[i]].ReadBytes > a.Groups[keys[j]].ReadBytes })
		fmt.Fprintln(w)
		barW := groupBarWidth(st)
		top := a.Groups[keys[0]].ReadBytes
		for _, k := range keys {
			t := a.Groups[k]
			label := k
			if label == "" {
				label = "(unknown)"
			}
			filled := 0
			if top > 0 && barW > 0 {
				filled = int(float64(t.ReadBytes) / float64(top) * float64(barW))
				if filled == 0 {
					filled = 1
				}
			}
			bar := st.Cyan(strings.Repeat("█", filled)) + st.Dim(strings.Repeat("░", barW-filled))
			grow(w, st, label, fmt.Sprintf("%s %d calls · %s read", bar, t.Calls, FormatBytes(t.ReadBytes)))
		}
	}
	if len(a.Top) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, st.Dim("top saves"))
		for _, r := range a.Top {
			targetW := st.Width() - 19
			if targetW < 8 {
				targetW = 8
			}
			fmt.Fprintf(w, "  %s  %s %s\n",
				st.Dim(term.PadLeft(FormatBytes(r.ReadBytes-r.EmittedBytes), 9)),
				term.PadRight(r.Cmd, 5),
				term.TruncMiddle(r.Target, targetW))
		}
	}
}

func downloadSeconds(a stats.Aggregate) float64 {
	if a.Totals.WallMS <= 0 || a.Totals.NetworkBytes <= 0 {
		return 0
	}
	throughput := float64(a.Totals.NetworkBytes) / (float64(a.Totals.WallMS) / 1000)
	cacheBytes := float64(a.Totals.ReadBytes - a.Totals.NetworkBytes)
	if throughput <= 0 || cacheBytes <= 0 {
		return 0
	}
	return cacheBytes / throughput
}

// FormatDuration renders seconds as a compact human duration.
func FormatDuration(s float64) string {
	if s <= 0 {
		return "0s"
	}
	d := time.Duration(s * float64(time.Second))
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// FormatBytes renders a byte count with binary units.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// FormatCount renders a large count compactly.
func FormatCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ---------- history ----------

// History renders `whyfail gain --history`: one row per recorded call, with the
// target middle-truncated so the useful end survives a narrow terminal.
func History(w io.Writer, st term.Style, recs []stats.Record) {
	if len(recs) == 0 {
		fmt.Fprintln(w, "no recorded calls yet")
		return
	}
	const (
		whenW   = 16 // 2006-01-02 15:04
		cmdW    = 12
		bytesW  = 24
		gapW    = 2 + whenW + 1 + cmdW + 1 + bytesW + 1
		minName = 12
	)
	targetW := st.Width() - gapW
	// On a narrow terminal there is no room for a target column at all; drop it
	// rather than squeezing every line past the width.
	showTarget := targetW >= minName
	fmt.Fprintf(w, "%s %s\n", st.Bold("recent calls"), st.Dim(fmt.Sprintf("(%d, newest last)", len(recs))))
	if summary := callSummary(st, recs); summary != "" {
		fmt.Fprintf(w, "  %s %s\n", st.Dim("calls/day"), term.Trunc(summary, st.Width()-13))
	}
	fmt.Fprintln(w)
	header := fmt.Sprintf("  %s %s %s",
		st.Dim(term.PadRight("WHEN", whenW)),
		st.Dim(term.PadRight("CMD", cmdW)),
		st.Dim(term.PadLeft("READ → EMITTED", bytesW)))
	if showTarget {
		header += " " + st.Dim("TARGET")
	}
	fmt.Fprintln(w, header)
	for _, r := range recs {
		line := fmt.Sprintf("  %s %s %s",
			st.Dim(r.TS.Format("2006-01-02 15:04")),
			term.PadRight(term.Trunc(r.Cmd, cmdW), cmdW),
			term.PadLeft(fmt.Sprintf("%s → %s", FormatBytes(r.ReadBytes), FormatBytes(r.EmittedBytes)), bytesW))
		if showTarget {
			line += " " + st.Dim(term.TruncMiddle(r.Target, targetW))
		}
		fmt.Fprintln(w, line)
	}
}

// ---------- search ----------

// Search renders a search.ci result: a headline count, up to ten matching jobs,
// and the known bug when the search found one.
func Search(w io.Writer, st term.Style, matches int, jobs []string, knownBug, maxAge string) {
	fmt.Fprintf(w, "%s %s\n", st.Bold(fmt.Sprintf("%d matching jobs", matches)),
		st.Dim("in the last "+maxAge))
	shown := jobs
	if len(shown) > 10 {
		shown = shown[:10]
	}
	nameW := st.Width() - 2
	if nameW < 12 {
		nameW = 12
	}
	for _, j := range shown {
		fmt.Fprintf(w, "  %s\n", term.TruncMiddle(j, nameW))
	}
	if more := len(jobs) - len(shown); more > 0 {
		fmt.Fprintf(w, "  %s\n", st.Dim(fmt.Sprintf("… and %d more", more)))
	}
	if knownBug != "" {
		fmt.Fprintln(w)
		row(w, st, "KNOWN BUG", st.Cyan(knownBug))
	}
}

// ---------- artifact ----------

// ArtifactEntry is one object in an artifact listing.
type ArtifactEntry struct {
	Name string
	Size int64
}

// ArtifactList renders a prefix listing, bounded by the caller. truncated says
// the listing was cut, so the totals line can say so instead of inventing a
// total it never read.
func ArtifactList(w io.Writer, st term.Style, objs []ArtifactEntry, truncated bool) {
	var total int64
	nameW := st.Width() - 14
	if nameW < 12 {
		nameW = 12
	}
	for _, o := range objs {
		total += o.Size
		fmt.Fprintf(w, "  %s  %s\n", st.Dim(term.PadLeft(FormatBytes(o.Size), 9)),
			term.TruncMiddle(o.Name, nameW))
	}
	if truncated {
		fmt.Fprintf(w, "  %s\n", st.Dim(fmt.Sprintf(
			"%d of at least %d objects, %s shown · use --limit 0 for all",
			len(objs), len(objs)+1, FormatBytes(total))))
		return
	}
	fmt.Fprintf(w, "  %s\n", st.Dim(fmt.Sprintf("%d objects, %s total", len(objs), FormatBytes(total))))
}

// ArtifactInfo is what `whyfail artifact` knows about one object.
type ArtifactInfo struct {
	Name        string
	Size        int64
	ContentType string
	Updated     string
	Data        []byte
	IsLog       bool
	Truncated   bool
	// Limit is the read cap that produced Truncated, reported to the reader.
	Limit int64
}

// Artifact renders one object's metadata plus a bounded excerpt. A build log is
// shown as a tail; anything else textual as the head of the object.
func Artifact(w io.Writer, st term.Style, a ArtifactInfo) {
	fmt.Fprintln(w, st.Bold(a.Name))
	row(w, st, "SIZE", FormatBytes(a.Size))
	row(w, st, "TYPE", a.ContentType)
	row(w, st, "UPDATED", a.Updated)

	if a.IsLog {
		lines := strings.Split(strings.TrimRight(string(a.Data), "\n"), "\n")
		if len(lines) > 120 {
			lines = lines[len(lines)-120:]
		}
		fmt.Fprintf(w, "\n%s\n%s\n", st.Dim("--- tail ---"), strings.Join(lines, "\n"))
		return
	}
	if len(a.Data) == 0 || !isText(a.Data) {
		return
	}
	fmt.Fprintf(w, "\n%s\n", st.Dim("--- excerpt ---"))
	_, _ = w.Write(a.Data)
	if a.Truncated {
		limit := a.Limit
		if limit <= 0 {
			limit = int64(len(a.Data))
		}
		fmt.Fprintf(w, "\n%s\n", st.Dim(fmt.Sprintf("... (%s truncated at %s)",
			FormatBytes(a.Size), FormatBytes(limit))))
	}
}

// isText reports whether b looks like text rather than an archive or image.
func isText(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// ---------- pull request ----------

// PRCard is one failing check on a PR head commit. Prow is false for a check
// that has no Prow job to look up at all — a GitHub Actions job, say. Health is
// nil when the job could not be looked up, usually a base-branch rename, which
// makes the synthesised job name wrong.
type PRCard struct {
	Check  string          `json:"check"`
	Job    string          `json:"job,omitempty"`
	Prow   bool            `json:"prow"`
	Health *health.Summary `json:"health"`
}

// PR renders a PR's failing checks against the shared gutter, so the check
// column lines up with every other card.
func PR(w io.Writer, st term.Style, org, repo, num, title string, cards []PRCard) {
	fmt.Fprintf(w, "%s/%s%s  %s\n\n", org, repo, st.Cyan("#"+num), title)
	if len(cards) == 0 {
		fmt.Fprintln(w, "no failing checks on the head commit")
		return
	}
	jobW := st.Width() - 44
	if jobW < 24 {
		jobW = 24
	}
	if jobW > 60 {
		jobW = 60
	}
	bad := 0
	for _, c := range cards {
		fmt.Fprint(w, PRCardLine(st, c, jobW))
		if c.Health != nil && c.Health.Verdict.IsBad() {
			bad++
		}
	}
	if bad > 0 && bad == len(cards) {
		fmt.Fprintf(w, "\n  %s\n", st.Yellow("all failing jobs have poor history — the PR is probably not the cause"))
	}
}

// cardLabelW is the width of the verdict column: icon, space, nine-char label.
const cardLabelW = 11

// PRCardLine renders one check. A nil Health is reported as an unknown Prow job
// or as a check with no Prow job behind it, never as healthy. The detail column
// is dropped rather than overflowing a narrow terminal.
func PRCardLine(st term.Style, c PRCard, jobW int) string {
	name := c.Job
	if name == "" {
		name = c.Check
	}
	name = term.PadRight(term.TruncMiddle(name, jobW), jobW)

	detailW := st.Width() - (2 + jobW + 1 + cardLabelW + 1)
	note := func(s string) string {
		if detailW < 8 {
			return ""
		}
		return " " + st.Dim(term.Trunc(s, detailW))
	}

	if c.Health == nil {
		if !c.Prow {
			return fmt.Sprintf("  %s %s%s\n", name,
				st.Red(term.PadRight("✗ FAILED", cardLabelW)),
				note("no Prow job for this check"))
		}
		return fmt.Sprintf("  %s %s%s\n", name,
			st.Dim(term.PadRight("? UNKNOWN", cardLabelW)),
			note("no history in this bucket"))
	}
	icon, label := VerdictLabel(c.Health.Verdict)
	line := fmt.Sprintf("  %s %s %s",
		name,
		VerdictText(st, c.Health.Verdict, icon+" "+term.PadRight(label, 9)),
		st.Dim(health.FormatRate(c.Health.PassRate)))
	if extra := fmt.Sprintf("%d runs", c.Health.Runs); detailW >= 8+len(extra) {
		line += st.Dim(" pass (" + extra + ")")
	}
	return line + "\n"
}

// verdictOrder is the order verdicts are listed in: worst first.
var verdictOrder = []health.Verdict{
	health.VerdictBroken, health.VerdictFlaky, health.VerdictDegraded,
	health.VerdictHealthy, health.VerdictUnknown,
}

// verdictCounts renders "3 BROKEN · 9 FLAKY · 1 HEALTHY", coloured per verdict.
// A verdict this build does not know is still shown, rather than being counted
// and then silently dropped.
func verdictCounts(st term.Style, counts map[string]int) string {
	var parts []string
	known := make(map[string]bool, len(verdictOrder))
	for _, v := range verdictOrder {
		known[string(v)] = true
		if n := counts[string(v)]; n > 0 {
			parts = append(parts, verdictStyle(st, v, fmt.Sprintf("%d %s", n, v)))
		}
	}
	var unknown []string
	for k, n := range counts {
		if !known[k] && n > 0 {
			unknown = append(unknown, fmt.Sprintf("%d %s", n, k))
		}
	}
	sort.Strings(unknown)
	parts = append(parts, unknown...)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, st.Dim(" · "))
}

// milestones are the round numbers the "next" row counts up to.
var milestones = []int64{
	1 << 20, 10 << 20, 100 << 20,
	1 << 30, 10 << 30, 100 << 30,
}

// nextMilestone returns the next round total above saved, or 0 when there is
// nothing to celebrate yet.
func nextMilestone(saved int64) int64 {
	if saved <= 0 {
		return 0
	}
	for _, m := range milestones {
		if saved < m {
			return m
		}
	}
	return 0
}

// groupBarValueW is roughly the width of " 12 calls · 47.2 MB read", which the
// bar shares its line with.
const groupBarValueW = 26

// groupBarWidth is how many cells a group bar may use, leaving room for the
// label and the counts on the same line.
func groupBarWidth(st term.Style) int {
	w := st.Width() - savingsGutter - groupBarValueW
	if w < 4 {
		return 0
	}
	if w > 24 {
		return 24
	}
	return w
}

// callSummary describes the span the listed calls cover: how many calls, over
// how many days, and the busiest day. A sparkline was tried here first and read
// as a row of underscores on a short range, so it states the numbers instead.
func callSummary(st term.Style, recs []stats.Record) string {
	if len(recs) == 0 {
		return ""
	}
	day := func(t time.Time) time.Time {
		y, m, d := t.UTC().Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	counts := map[time.Time]int{}
	first, last := day(recs[0].TS), day(recs[0].TS)
	for _, r := range recs {
		d := day(r.TS)
		counts[d]++
		if d.Before(first) {
			first = d
		}
		if d.After(last) {
			last = d
		}
	}
	busiest, most := first, 0
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		if counts[d] > most {
			busiest, most = d, counts[d]
		}
	}
	span := int(last.Sub(first).Hours()/24) + 1
	return fmt.Sprintf("%d over %s · busiest %s (%d)",
		len(recs), plural(span, "1 day", fmt.Sprintf("%d days", span)),
		busiest.Format("2 Jan"), most)
}

// Fun writes the optional, less serious statistics behind `gain --fun`. The
// lines stay short enough to survive a narrow terminal, because the numbers are
// the point and a folded joke is not funny.
func Fun(w io.Writer, st term.Style, recs []stats.Record) {
	if len(recs) == 0 {
		return
	}
	var (
		worst   stats.Record
		judged  int
		passSum float64
		hours   = map[int]int{}
		busiest = -1
		busyN   int
	)
	for _, r := range recs {
		if r.ReadBytes > worst.ReadBytes {
			worst = r
		}
		if r.PassRate > 0 && (r.Verdict == "FLAKY" || r.Verdict == "BROKEN") {
			judged++
			passSum += r.PassRate
		}
		hours[r.TS.UTC().Hour()]++
	}
	// Pick the busiest hour after counting, so a tie resolves to the earliest
	// hour rather than to whichever record happened to come last.
	for h, n := range hours {
		if n > busyN || (n == busyN && (busiest < 0 || h < busiest)) {
			busiest, busyN = h, n
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, st.Dim("for fun"))
	if worst.ReadBytes > 0 {
		prefix := FormatBytes(worst.ReadBytes) + " · "
		room := st.Width() - 21 - 2 - 1 - len(prefix)
		if room < 8 {
			room = 8
		}
		grow(w, st, "worst", prefix+st.Dim(term.TruncMiddle(worst.Target, room)))
	}
	if judged > 0 {
		grow(w, st, "retest roulette", fmt.Sprintf("%d flaky · blind retest wins %.0f%%",
			judged, passSum/float64(judged)*100))
	}
	if busiest >= 0 {
		grow(w, st, "hours", fmt.Sprintf("%02d:00 UTC · %d of %d calls",
			busiest, busyN, len(recs)))
	}
}
