// Package render formats whyfail output for humans.
package render

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/run"
	"github.com/sebrandon1/whyfail/internal/stats"
)

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

// Digest writes a human-readable digest.
func Digest(w io.Writer, d *run.Digest) {
	if d.Job != nil && d.Job.Health != nil {
		h := d.Job.Health
		icon, label := VerdictLabel(h.Verdict)
		fmt.Fprintf(w, "%-36s %s %s", d.Run.JobName, icon, label)
		if h.Pass+h.Fail > 0 {
			fmt.Fprintf(w, "  (%s pass, %d runs since %s)",
				health.FormatRate(h.PassRate), h.Runs, h.From.Format("2006-01-02"))
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, d.Run.JobName)
	}

	fmt.Fprintf(w, "run %s  %s", d.Run.ID, strings.ToUpper(d.Run.Result))
	if d.Run.Timing.DurationS > 0 {
		fmt.Fprintf(w, "  %s", FormatDuration(d.Run.Timing.DurationS))
		if d.Run.Timing.TimeoutS > 0 {
			fmt.Fprintf(w, " (%.0f%% of %s timeout)", d.Run.Timing.PctOfTimeout, FormatDuration(d.Run.Timing.TimeoutS))
		}
	}
	fmt.Fprintln(w)

	if pr := d.Run.PR; pr != nil && pr.Number != 0 {
		fmt.Fprintf(w, "pr   %s/%s#%d", pr.Org, pr.Repo, pr.Number)
		if pr.Author != "" {
			fmt.Fprintf(w, "  by %s", pr.Author)
		}
		if pr.Title != "" {
			fmt.Fprintf(w, "  %q", truncate(pr.Title, 90))
		}
		fmt.Fprintln(w)
	}

	var meta []string
	if d.Run.Cluster != "" {
		meta = append(meta, "cluster "+d.Run.Cluster)
	}
	if d.Run.Namespace != "" {
		meta = append(meta, "ns "+d.Run.Namespace)
	}
	if d.Run.RerunCommand != "" {
		meta = append(meta, d.Run.RerunCommand)
	}
	if len(meta) > 0 {
		fmt.Fprintln(w, "     "+strings.Join(meta, " · "))
	}
	if d.Run.Reason != "" {
		fmt.Fprintln(w, "     reason: "+d.Run.Reason)
	}
	fmt.Fprintln(w)

	if s := d.Run.Steps; s.Total > 0 {
		fmt.Fprintf(w, "steps (%d): %d failed\n", s.Total, len(s.Failed))
		for _, f := range s.Failed {
			fmt.Fprintf(w, "  ✗ %-56s %8s\n", f.Name, FormatDuration(f.DurationS))
			if f.Error != "" {
				fmt.Fprintf(w, "      %s\n", truncate(f.Error, 150))
			}
		}
		if len(s.Longest) > 0 {
			var parts []string
			for _, l := range s.Longest {
				parts = append(parts, fmt.Sprintf("%s %s", l.Name, FormatDuration(l.DurationS)))
			}
			fmt.Fprintf(w, "  longest: %s\n", strings.Join(parts, " · "))
		}
		fmt.Fprintln(w)
	}

	if t := d.Tests; t != nil {
		fmt.Fprintf(w, "tests: %d pass / %d fail / %d skip", t.Counts.Passed, t.Counts.Failed, t.Counts.Skipped)
		if t.Counts.Flaked > 0 {
			fmt.Fprintf(w, " / %d flaked", t.Counts.Flaked)
		}
		if t.Source != "" {
			fmt.Fprintf(w, "  [%s]", t.Source)
		}
		fmt.Fprintln(w)
		for _, f := range t.Failed {
			label := "inform"
			if f.Blocking {
				label = "BLOCK"
			}
			fmt.Fprintf(w, "  %-6s %s\n", label, truncate(f.Name, 110))
			if f.Error != "" {
				for _, line := range strings.Split(f.Error, "\n") {
					fmt.Fprintf(w, "         %s\n", truncate(line, 130))
				}
			}
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
			fmt.Fprintf(w, "monitors: %d failures in %d cause(s)\n", total, len(groups))
			for i, g := range groups {
				if i >= 6 {
					fmt.Fprintf(w, "  ... and %d more cause(s)\n", len(groups)-6)
					break
				}
				fmt.Fprintf(w, "  [%dx] %s\n", g.Count, truncate(g.Signature, 120))
			}
		}
		fmt.Fprintln(w)
	}

	if in := d.Infra; in != nil {
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
			parts = append(parts, fmt.Sprintf("available_at_acquire=%d", in.Lease.AvailableAtAcquire))
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
			fmt.Fprintln(w, "infra: "+strings.Join(parts, " · "))
		}
		if in.Execution != nil {
			fmt.Fprintf(w, "       ci-operator: success=%v errors=%d duration=%s\n",
				in.Execution.Success, in.Execution.ErrorCount, FormatDuration(in.Execution.DurationS))
		}
		fmt.Fprintln(w)
	}

	if a := d.Artifacts; a != nil && a.Count > 0 {
		fmt.Fprintf(w, "artifacts: %d objects, %s total\n", a.Count, FormatBytes(a.TotalBytes))
		for _, n := range a.Notable {
			name := n.Short
			if name == "" {
				name = n.Name
			}
			fmt.Fprintf(w, "  %-10s %s (%s)\n", FormatBytes(n.Bytes), truncate(name, 90), n.Why)
		}
		fmt.Fprintln(w)
	}

	if d.Signals != nil {
		var parts []string
		if d.Signals.Sippy != "" {
			parts = append(parts, "sippy="+d.Signals.Sippy)
		}
		if s := d.Signals.SearchCI; s != nil {
			p := fmt.Sprintf("search.ci=%d matches in 48h", s.Matches48h)
			if s.KnownBug != "" {
				p += " (" + s.KnownBug + ")"
			}
			parts = append(parts, p)
		}
		if len(parts) > 0 {
			fmt.Fprintln(w, "signals: "+strings.Join(parts, " · "))
		}
	}

	if d.NextAction != "" {
		fmt.Fprintln(w, "next: "+d.NextAction)
	}
	fmt.Fprintln(w, "url:  "+d.Run.URL)
}

// Job writes a job health card.
func Job(w io.Writer, s *health.Summary) {
	icon, label := VerdictLabel(s.Verdict)
	fmt.Fprintf(w, "%s  %s %s\n", s.Job, icon, label)
	if s.Runs == 0 {
		fmt.Fprintln(w, "  no runs found in the Prow index (job may be renamed or never have run)")
		return
	}
	fmt.Fprintf(w, "  window %s → %s · %d runs · %d pass / %d fail / %d abort / %d missing\n",
		s.From.Format("2006-01-02"), s.To.Format("2006-01-02"), s.Runs, s.Pass, s.Fail, s.Abort, s.Missing)
	fmt.Fprintf(w, "  pass rate %s (recent %s) · streak %d · ",
		health.FormatRate(s.PassRate), health.FormatRate(s.RecentPassRate), s.Streak)
	if s.LastPass != nil {
		fmt.Fprintf(w, "last pass %s", s.LastPass.Format("2006-01-02"))
	} else {
		fmt.Fprint(w, "last pass: none in window")
	}
	if s.DistinctPRs > 0 {
		fmt.Fprintf(w, " · %d distinct PRs failing", s.DistinctPRs)
	}
	fmt.Fprintln(w)
	switch s.Verdict {
	case health.VerdictBroken:
		fmt.Fprintln(w, "  → retesting will not help; this job is red for everyone")
	case health.VerdictFlaky:
		fmt.Fprintln(w, "  → a retest is reasonable")
	case health.VerdictHealthy:
		fmt.Fprintln(w, "  → this job passes reliably")
	}
}

// Gain writes the savings summary.
func Gain(w io.Writer, a stats.Aggregate, bytesPerToken int, baseline time.Duration) {
	if a.Totals.Calls == 0 {
		fmt.Fprintln(w, "no recorded calls yet")
		return
	}
	savedBytes := a.Totals.ReadBytes - a.Totals.EmittedBytes
	if savedBytes < 0 {
		savedBytes = 0
	}
	fmt.Fprintf(w, "whyfail saved ~%s across %d calls", FormatBytes(savedBytes), a.Totals.Calls)
	if !a.Since.IsZero() {
		fmt.Fprintf(w, " (since %s)", a.Since.Format("2006-01-02"))
	}
	fmt.Fprintln(w)

	pct := 0.0
	if a.Totals.ReadBytes > 0 {
		pct = float64(a.Totals.EmittedBytes) / float64(a.Totals.ReadBytes) * 100
	}
	fmt.Fprintf(w, "\n  calls          %6d        cache hits %d\n", a.Totals.Calls, a.Totals.CacheHits)
	fmt.Fprintf(w, "  artifacts read %10s → emitted %8s (%.1f%% of input)\n",
		FormatBytes(a.Totals.ReadBytes), FormatBytes(a.Totals.EmittedBytes), pct)
	if a.Totals.InventoryBytes > 0 {
		fmt.Fprintf(w, "  full trees     %10s      (upper bound, not all of it would be read)\n", FormatBytes(a.Totals.InventoryBytes))
	}
	tokens := savedBytes / int64(max(bytesPerToken, 1))
	fmt.Fprintf(w, "  ~tokens saved  %10s\n", FormatCount(tokens))

	if dl := downloadSeconds(a); dl > 0 {
		fmt.Fprintf(w, "  ~download time saved %s (measured throughput)\n", FormatDuration(dl))
	}
	if baseline > 0 {
		manual := time.Duration(a.Totals.Calls) * baseline
		tool := time.Duration(a.Totals.WallMS) * time.Millisecond
		if manual > tool {
			fmt.Fprintf(w, "  ~manual time saved   %s (vs %s baseline per call; modelled)\n",
				FormatDuration((manual - tool).Seconds()), baseline)
		}
	}
	fmt.Fprintln(w, "\n  (measured numbers come from recorded calls; ~ marks modelled estimates)")

	if len(a.Groups) > 0 {
		keys := make([]string, 0, len(a.Groups))
		for k := range a.Groups {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return a.Groups[keys[i]].ReadBytes > a.Groups[keys[j]].ReadBytes })
		fmt.Fprintln(w)
		for _, k := range keys {
			t := a.Groups[k]
			label := k
			if label == "" {
				label = "(unknown)"
			}
			fmt.Fprintf(w, "  %-40s %4d calls  %10s read\n", label, t.Calls, FormatBytes(t.ReadBytes))
		}
	}
	if len(a.Top) > 0 {
		fmt.Fprintln(w, "\n  top saves")
		for _, r := range a.Top {
			fmt.Fprintf(w, "    %10s  %-5s %s\n", FormatBytes(r.ReadBytes-r.EmittedBytes), r.Cmd, truncate(r.Target, 90))
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
