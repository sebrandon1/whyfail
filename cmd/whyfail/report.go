package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sebrandon1/whyfail/internal/api"
	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/parallel"
	"github.com/sebrandon1/whyfail/internal/prow"
	"github.com/sebrandon1/whyfail/internal/stats"
)

// reportFanOut bounds how many jobs are collected at once. Each Collect fans out
// internally, so the outer width stays small.
const reportFanOut = 4

// deckJobHistory is Prow's run-history page for a presubmit job. It is what a
// board row links to, so a claim on the page can be checked against the runs.
const deckJobHistory = "https://prow.ci.openshift.org/job-history/gs/" +
	prow.PublicBucket + "/pr-logs/directory/"

// reportJob is one row of the board. It is a flattened health.Summary plus the
// repository and the history URL, so a reader of the JSON does not need to know
// health's shape by heart.
type reportJob struct {
	Job            string         `json:"job"`
	Repo           string         `json:"repo,omitempty"`
	Verdict        health.Verdict `json:"verdict"`
	PassRate       float64        `json:"pass_rate"`
	RecentPassRate float64        `json:"recent_pass_rate"`
	Runs           int            `json:"runs"`
	Pass           int            `json:"pass"`
	Fail           int            `json:"fail"`
	Abort          int            `json:"abort"`
	Missing        int            `json:"missing"`
	Streak         int            `json:"streak"`
	LastPassAt     *time.Time     `json:"last_pass_at"`
	DistinctPRs    int            `json:"distinct_prs_failing"`
	URL            string         `json:"url"`
	// Error is set when the lookup itself failed, so a transient bucket error
	// does not silently look like a job that nobody has run.
	Error string `json:"error,omitempty"`
}

// reportOut is the snapshot the board loads.
type reportOut struct {
	Schema      string      `json:"schema"`
	Kind        string      `json:"kind"`
	GeneratedAt time.Time   `json:"generated_at"`
	Window      string      `json:"window"`
	Bucket      string      `json:"bucket"`
	Jobs        []reportJob `json:"jobs"`
}

// historyJob is one day's point for a job: enough to draw a trend without
// storing a run list per day.
type historyJob struct {
	Verdict  health.Verdict `json:"verdict"`
	PassRate float64        `json:"pass_rate"`
	Runs     int            `json:"runs"`
}

type historyPoint struct {
	Date string                `json:"date"`
	Jobs map[string]historyJob `json:"jobs"`
}

type historyOut struct {
	Schema string         `json:"schema"`
	Points []historyPoint `json:"points"`
}

func newReportCmd(g *globals) *cobra.Command {
	var jobsFile, outFile, historyFile string
	var keep int
	cmd := &cobra.Command{
		Use:   "report [--jobs FILE] [--out FILE] [--history FILE]",
		Short: "Collect job health for a curated job list as one JSON snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			names, duplicates, err := readJobList(jobsFile)
			if err != nil {
				return err
			}
			if duplicates > 0 {
				fmt.Fprintf(os.Stderr, "whyfail report: ignored %d duplicate job name(s) in %s\n", duplicates, jobsFile)
			}
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()

			jobs := make([]reportJob, len(names))
			_ = parallel.Do(len(names), reportFanOut, func(i int) error {
				sum, err := health.Collect(ctx, c, g.bucket, names[i], g.window)
				switch {
				case err == nil:
					jobs[i] = reportJob{
						Job:            sum.Job,
						Repo:           sum.Repo(),
						Verdict:        sum.Verdict,
						PassRate:       sum.PassRate,
						RecentPassRate: sum.RecentPassRate,
						Runs:           sum.Runs,
						Pass:           sum.Pass,
						Fail:           sum.Fail,
						Abort:          sum.Abort,
						Missing:        sum.Missing,
						Streak:         sum.Streak,
						LastPassAt:     sum.LastPass,
						DistinctPRs:    sum.DistinctPRs,
						URL:            deckJobHistory + url.PathEscape(sum.Job),
					}
				case errors.Is(err, health.ErrJobNotFound):
					// A curated name that matches nothing is a mistake in the
					// list, not a reason to drop the whole board.
					jobs[i] = reportJob{
						Job:     names[i],
						Verdict: health.VerdictUnknown,
						URL:     deckJobHistory + url.PathEscape(names[i]),
					}
				default:
					jobs[i] = reportJob{
						Job:     names[i],
						Verdict: health.VerdictUnknown,
						URL:     deckJobHistory + url.PathEscape(names[i]),
						Error:   err.Error(),
					}
				}
				return nil
			})
			sortReportJobs(jobs)

			doc := reportOut{
				Schema:      api.SchemaV1,
				Kind:        "flaky-report",
				GeneratedAt: time.Now().UTC().Truncate(time.Second),
				Window:      windowLabel(g.window),
				Bucket:      g.bucket,
				Jobs:        jobs,
			}
			var buf bytes.Buffer
			writeJSON(&buf, doc)

			// A snapshot written to a file is shown to nobody, so only the stdout
			// case counts as emitted bytes.
			rec := stats.Record{Cmd: "report", Target: jobsFile}
			if outFile == "" {
				g.emit(&buf, rec, c, start)
			} else {
				if err := writeFile(outFile, buf.Bytes()); err != nil {
					return err
				}
				g.record(rec, c, start)
			}

			if historyFile != "" {
				if err := appendHistory(historyFile, doc.GeneratedAt, jobs, keep); err != nil {
					return err
				}
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&jobsFile, "jobs", "report/jobs.txt", "file listing one job name per line (# comments allowed)")
	cmd.Flags().StringVar(&outFile, "out", "", "write the snapshot here instead of stdout")
	cmd.Flags().StringVar(&historyFile, "history", "", "append today's point to this history file")
	cmd.Flags().IntVar(&keep, "keep", 180, "days of history to retain")
	return cmd
}

// windowLabel says what the snapshot summarised, in the units a reader thinks
// in: "720h" rather than "720h0m0s", and words rather than "0s" when the window
// is the bucket's whole retention.
func windowLabel(d time.Duration) string {
	switch {
	case d <= 0:
		return "all retained"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return d.String()
	}
}

// readJobList reads one job name per line, ignoring blank lines and comments.
// A name repeated in the file is counted and kept once, so a hand-editing slip
// cannot put the same job on the board twice.
func readJobList(path string) ([]string, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read job list: %w", err)
	}
	var out []string
	seen := map[string]bool{}
	duplicates := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if seen[line] {
			duplicates++
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil, 0, fmt.Errorf("%s lists no jobs", path)
	}
	return out, duplicates, nil
}

// verdictRank orders rows worst first, so the board needs no extra sort key.
func verdictRank(v health.Verdict) int {
	switch v {
	case health.VerdictBroken:
		return 0
	case health.VerdictFlaky:
		return 1
	case health.VerdictDegraded:
		return 2
	case health.VerdictUnknown:
		return 3
	case health.VerdictHealthy:
		return 4
	}
	return 3
}

// sortReportJobs puts the jobs that need attention first: verdict severity, then
// failing runs, then name, so the order is stable for a given snapshot.
func sortReportJobs(jobs []reportJob) {
	sort.SliceStable(jobs, func(i, j int) bool {
		if a, b := verdictRank(jobs[i].Verdict), verdictRank(jobs[j].Verdict); a != b {
			return a < b
		}
		if jobs[i].Fail != jobs[j].Fail {
			return jobs[i].Fail > jobs[j].Fail
		}
		return jobs[i].Job < jobs[j].Job
	})
}

// appendHistory writes today's point into the history file, replacing an earlier
// point for the same day so a re-run is idempotent, and drops points older than
// keep days.
func appendHistory(path string, at time.Time, jobs []reportJob, keep int) error {
	h := &historyOut{Schema: api.SchemaV1}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, h); err != nil {
			return fmt.Errorf("read history %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read history %s: %w", path, err)
	}
	h.Schema = api.SchemaV1

	day := at.Format("2006-01-02")
	point := historyPoint{Date: day, Jobs: map[string]historyJob{}}
	for _, j := range jobs {
		point.Jobs[j.Job] = historyJob{Verdict: j.Verdict, PassRate: j.PassRate, Runs: j.Runs}
	}
	// A re-run on the same day replaces that day's point rather than adding a
	// second one the chart would draw as a spike.
	merged := []historyPoint{point}
	for _, p := range h.Points {
		if p.Date != day {
			merged = append(merged, p)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Date < merged[j].Date })
	if keep > 0 && len(merged) > keep {
		merged = merged[len(merged)-keep:]
	}
	h.Points = merged

	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}

// writeFile writes atomically, so a half-written snapshot never gets published.
func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".whyfail-report-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
