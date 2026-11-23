package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sebrandon1/whyfail/internal/api"
	"github.com/sebrandon1/whyfail/internal/claudecfg"
	"github.com/sebrandon1/whyfail/internal/enrich"
	"github.com/sebrandon1/whyfail/internal/gcs"
	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/hook"
	"github.com/sebrandon1/whyfail/internal/parallel"
	"github.com/sebrandon1/whyfail/internal/prow"
	"github.com/sebrandon1/whyfail/internal/render"
	"github.com/sebrandon1/whyfail/internal/run"
	"github.com/sebrandon1/whyfail/internal/stats"
	"github.com/sebrandon1/whyfail/internal/term"
)

var version = "dev"

type globals struct {
	jsonOut       bool
	noStats       bool
	cacheDir      string
	refresh       bool
	window        time.Duration
	bytesPerToken int
	baseline      time.Duration
	bucket        string
	colorMode     string
	width         int
	retries       int
	cacheMax      string
	cacheMaxBytes int64
	timeout       time.Duration
}

func main() {
	g := &globals{bytesPerToken: 4, baseline: 10 * time.Minute, colorMode: "auto", retries: 2, timeout: 2 * time.Minute}
	root := &cobra.Command{
		Use:           "whyfail",
		Short:         "Prow CI context for humans and Claude Code",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&g.jsonOut, "json", false, "emit JSON instead of human output")
	root.PersistentFlags().BoolVar(&g.noStats, "no-stats", false, "do not record invocation statistics")
	root.PersistentFlags().StringVar(&g.cacheDir, "cache-dir", "", "cache directory (default: user cache dir)")
	root.PersistentFlags().BoolVar(&g.refresh, "refresh", false, "ignore cached listings and objects")
	root.PersistentFlags().DurationVar(&g.window, "window", 0, "history window (e.g. 720h); 0 = all retained")
	root.PersistentFlags().StringVar(&g.bucket, "bucket", prow.PublicBucket, "GCS bucket for job history")
	root.PersistentFlags().IntVar(&g.bytesPerToken, "bytes-per-token", 4, "bytes per token estimate for gain")
	root.PersistentFlags().DurationVar(&g.baseline, "baseline", 10*time.Minute, "modelled manual time per call for gain")
	root.PersistentFlags().StringVar(&g.colorMode, "color", "auto", "colourise human output: auto, always or never")
	root.PersistentFlags().IntVar(&g.width, "width", 0, "output width in columns (default: $COLUMNS or 100)")
	root.PersistentFlags().IntVar(&g.retries, "retries", 2, "retry transient HTTP failures (429, 5xx) this many times")
	root.PersistentFlags().StringVar(&g.cacheMax, "cache-max", "512MB", "object cache size limit (e.g. 512MB, 2GB); 0 disables pruning")
	root.PersistentFlags().DurationVar(&g.timeout, "timeout", 2*time.Minute, "overall deadline for a command (e.g. 30s, 5m); 0 disables")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseByteSize(g.cacheMax)
		if err != nil {
			return fmt.Errorf("--cache-max: %w", err)
		}
		g.cacheMaxBytes = n
		return nil
	}

	root.AddCommand(
		newRunCmd(g), newJobCmd(g), newFailuresCmd(g), newLogCmd(g), newArtifactCmd(g),
		newSearchCmd(g), newGainCmd(g), newHookCmd(), newDoctorCmd(g), newPRCmd(g), newMCPCmd(g),
		newInitCmd(g), newReportCmd(g),
	)
	if err := root.Execute(); err != nil {
		// A run that failed under --exit-code is a result, not a tool error: it
		// exits 2 and prints nothing extra, so the digest stays the whole story.
		if !errors.Is(err, errRunFailed) {
			fmt.Fprintln(os.Stderr, "whyfail:", err)
		}
		os.Exit(exitStatus(err))
	}
}

// errRunFailed reports that the run the caller asked about did not pass. It is a
// sentinel so the exit status can be decided without inspecting messages.
var errRunFailed = errors.New("run failed")

// exitStatus maps a command error to the process exit status: 0 for success, 2
// for a run that failed under --exit-code, 1 for every tool error.
func exitStatus(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errRunFailed):
		return 2
	default:
		return 1
	}
}

// ctx returns a context bounded by --timeout. A zero timeout means no
// deadline, for a user on a slow link.
func (g *globals) ctx() (context.Context, context.CancelFunc) {
	if g.timeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), g.timeout)
}

// timeoutError replaces a bare deadline overrun with a message that names the
// operation and the flag to raise. Every other error passes through untouched.
func (g *globals) timeoutError(label string, err error) error {
	if err == nil || g.timeout <= 0 || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s timed out after %s (raise --timeout or narrow the request)", label, g.timeout)
}

// style resolves how human output should be painted. It inspects stdout rather
// than the render buffer, because rendering happens into memory.
func (g *globals) style() term.Style {
	return term.Config(os.Stdout, g.colorMode, g.width)
}

func newClient(g *globals) *gcs.Client {
	dir := g.cacheDir
	if dir == "" {
		dir = stats.Dir()
	}
	c := gcs.New(dir)
	c.Refresh = g.refresh
	c.Retries = g.retries
	pruneCache(c, g.cacheMaxBytes)
	return c
}

// pruneCache keeps the object cache bounded. It runs at most once a day, never
// under --refresh (a user debugging a bad cache must keep the evidence), and
// never fails the command.
func pruneCache(c *gcs.Client, maxBytes int64) {
	if maxBytes <= 0 || c.Refresh {
		return
	}
	stamp := filepath.Join(c.CacheDir, "last-prune")
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < 24*time.Hour {
		return
	}
	_, _, _ = c.Prune(maxBytes, time.Hour)
	_ = os.MkdirAll(c.CacheDir, 0o755)
	_ = os.WriteFile(stamp, nil, 0o644)
}

// parseByteSize accepts 512MB, 2GB, 100KB or a plain byte count.
func parseByteSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// emit writes the rendered output and records one stats entry.
func (g *globals) emit(buf *bytes.Buffer, rec stats.Record, c *gcs.Client, start time.Time) {
	_, _ = os.Stdout.Write(buf.Bytes())
	// Count visible bytes only, so escape sequences never inflate the savings.
	rec.EmittedBytes = int64(term.PlainBytes(buf.String()))
	g.record(rec, c, start)
}

// record appends one stats entry. It is best-effort: a failure to record must
// not fail a command, and the MCP path must never write anything but JSON-RPC
// frames to stdout, so the write goes to the stats file alone.
func (g *globals) record(rec stats.Record, c *gcs.Client, start time.Time) {
	if g.noStats || !stats.Enabled() {
		return
	}
	rec.TS = time.Now().UTC()
	if c != nil {
		rec.ReadBytes = c.ServedBytes
		rec.NetworkBytes = c.NetworkBytes
		rec.CacheHits = c.CacheHits
		rec.Missing = c.Missing
	}
	rec.WallMS = time.Since(start).Milliseconds()
	_ = stats.Append(rec)
}

// The --json shapes. Every document carries the schema key first so a consumer
// can assert one thing across all commands; field names are stable and additive.
type failuresOut struct {
	Schema string        `json:"schema"`
	Run    *run.RunInfo  `json:"run"`
	Tests  *run.Tests    `json:"tests,omitempty"`
	Failed []run.StepRef `json:"failed_steps,omitempty"`
}

type logOut struct {
	Schema   string `json:"schema"`
	Artifact string `json:"artifact"`
	Lines    int    `json:"lines"`
	Text     string `json:"text"`
}

type artifactOut struct {
	Schema      string `json:"schema"`
	Object      string `json:"object"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	Updated     string `json:"updated"`
	Excerpt     string `json:"excerpt"`
	Truncated   bool   `json:"truncated"`
}

// artifactListOut is the listing form of `artifact`, which is a different
// document from the single-object one: `objects` present means a listing.
type artifactListOut struct {
	Schema     string                 `json:"schema"`
	Prefix     string                 `json:"prefix"`
	Objects    []render.ArtifactEntry `json:"objects"`
	TotalBytes int64                  `json:"total_bytes"`
	Truncated  bool                   `json:"truncated"`
}

type prOut struct {
	Schema  string          `json:"schema"`
	PR      string          `json:"pr"`
	Title   string          `json:"title"`
	SHA     string          `json:"sha"`
	Failing []render.PRCard `json:"failing"`
	// Warning carries a fan-out failure, which can only be a recovered panic:
	// the worker itself never returns an error.
	Warning string `json:"warning,omitempty"`
}

func writeJSON(buf *bytes.Buffer, v any) {
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ---------- run ----------

func newRunCmd(g *globals) *cobra.Command {
	var noHistory, search bool
	var noSippy, exitOnFailure bool
	cmd := &cobra.Command{
		Use:   "run <prow-url | gs://bucket/path>",
		Short: "Digest a single Prow job run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ref, err := prow.ParseRef(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()

			d, err := run.Build(ctx, c, ref, run.Options{History: !noHistory, Window: g.window})
			if err != nil {
				return err
			}

			sig := &run.Signals{}
			if !noSippy {
				sig.Sippy = enrich.SippyStatus(ctx, c.HTTP, ref.RunID)
			}
			if search {
				if q := topSignature(d); q != "" {
					if res, err := enrich.SearchCI(ctx, c.HTTP, q, "48h"); err == nil {
						sig.SearchCI = &run.SearchSig{Query: q, Matches48h: res.Matches, KnownBug: res.KnownBug}
					}
				}
			}
			if sig.Sippy != "" || sig.SearchCI != nil {
				d.Signals = sig
			}

			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, d)
			} else {
				render.Digest(&buf, d, g.style())
			}
			rec := stats.Record{Cmd: "run", Target: ref.Job + "/" + ref.RunID, Repo: repoOf(d, ref)}
			if d.Artifacts != nil {
				rec.InventoryBytes = d.Artifacts.TotalBytes
			}
			if d.Job != nil && d.Job.Health != nil {
				rec.Verdict = string(d.Job.Health.Verdict)
				rec.PassRate = d.Job.Health.PassRate
			}
			g.emit(&buf, rec, c, start)
			// Opt-in: the default stays exit 0, because the command is also read
			// interactively and by pipelines that do not expect a failure code.
			if exitOnFailure && d.Run != nil && d.Run.Result != "SUCCESS" {
				return errRunFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noHistory, "no-history", false, "skip job history (faster)")
	cmd.Flags().BoolVar(&search, "search", false, "look up the top failure signature on search.ci")
	cmd.Flags().BoolVar(&noSippy, "no-sippy", false, "skip the Sippy coverage probe")
	cmd.Flags().BoolVar(&exitOnFailure, "exit-code", false, "exit 2 if the run did not pass")
	return cmd
}

func topSignature(d *run.Digest) string {
	if d.Tests != nil {
		for _, f := range d.Tests.Failed {
			if f.Blocking {
				return f.Name
			}
		}
		if len(d.Tests.Failed) > 0 {
			return d.Tests.Failed[0].Name
		}
		if len(d.Tests.Monitors) > 0 {
			if s := d.Tests.Monitors[0].Signature; s != "" {
				return s
			}
			return d.Tests.Monitors[0].Name
		}
	}
	if d.Run != nil && len(d.Run.Steps.Failed) > 0 {
		return d.Run.Steps.Failed[0].Name
	}
	return ""
}

// ---------- job ----------

func newJobCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "job <job-name>",
		Short: "Job health over retained Prow history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()
			sum, err := health.Collect(ctx, c, g.bucket, args[0], g.window)
			if err != nil {
				if errors.Is(err, health.ErrJobNotFound) {
					return fmt.Errorf("job %q not found in %s (checked pr-logs/directory/ and logs/)", args[0], g.bucket)
				}
				return err
			}
			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, sum)
			} else {
				render.Job(&buf, sum, g.style())
			}
			// No Repo: a job name alone does not identify a repository — both the
			// org and the repo may contain the dashes the name is built from — so
			// this records under (unknown) rather than guessing.
			g.emit(&buf, stats.Record{Cmd: "job", Target: args[0], Verdict: string(sum.Verdict), PassRate: sum.PassRate}, c, start)
			return nil
		},
	}
}

// ---------- failures ----------

func newFailuresCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "failures <prow-url>",
		Short: "Failed tests and steps only",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ref, err := prow.ParseRef(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()
			d, err := run.Build(ctx, c, ref, run.Options{History: false})
			if err != nil {
				return err
			}
			out := failuresOut{Schema: api.SchemaV1, Run: d.Run, Tests: d.Tests, Failed: d.Run.Steps.Failed}

			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, out)
			} else {
				render.Failures(&buf, g.style(), d.Run.JobName, d.Run.Result, out.Failed, d.Tests)
			}
			g.emit(&buf, stats.Record{Cmd: "failures", Target: ref.Job + "/" + ref.RunID, Repo: repoOf(d, ref)}, c, start)
			return nil
		},
	}
}

// repoOf returns the org/repo a run belongs to. prowjob.json is authoritative;
// the object path covers a run whose prowjob.json is missing or periodic.
func repoOf(d *run.Digest, ref prow.Ref) string {
	if d != nil && d.Run != nil && d.Run.PR != nil && d.Run.PR.Org != "" && d.Run.PR.Repo != "" {
		return d.Run.PR.Org + "/" + d.Run.PR.Repo
	}
	return prow.Repo(ref.ObjectPath)
}

// ---------- log ----------

func newLogCmd(g *globals) *cobra.Command {
	var tail int
	var grep string
	var contextLines int
	cmd := &cobra.Command{
		Use:   "log <prow-url> <step> [--tail N] [--grep RE] [--context N]",
		Short: "Bounded view of a step's build log",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ref, err := prow.ParseRef(args[0])
			if err != nil {
				return err
			}
			step := args[1]
			// Compile before fetching: a typo in the pattern must not cost a
			// network round trip.
			var re *regexp.Regexp
			if grep != "" {
				re, err = regexp.Compile(grep)
				if err != nil {
					return fmt.Errorf("invalid --grep pattern: %w", err)
				}
			}
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()

			target, data, err := resolveStepLog(ctx, c, ref.Bucket, ref.ObjectPath, step)
			if err != nil {
				return err
			}
			doc := logDocument(target, data, tail, re, contextLines)

			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, doc)
			} else {
				buf.WriteString(doc.Text)
			}
			g.emit(&buf, stats.Record{Cmd: "log", Target: target, Repo: prow.Repo(ref.ObjectPath)}, c, start)
			return nil
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 120, "number of trailing lines (or matches, with --grep)")
	cmd.Flags().StringVar(&grep, "grep", "", "print only lines matching this regular expression")
	cmd.Flags().IntVar(&contextLines, "context", 0, "lines of context around each --grep match")
	return cmd
}

// logDocument selects the requested lines from a step log and wraps them in the
// document both `whyfail log` and the MCP prow_log tool return, so the two
// cannot drift.
func logDocument(target string, data []byte, tail int, re *regexp.Regexp, contextLines int) logOut {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out string
	printed := 0
	if re != nil {
		selected, total, kept := grepLog(lines, re, contextLines, tail)
		printed = len(selected)
		out = strings.Join(selected, "\n")
		if out != "" {
			out += "\n"
		}
		if further := total - kept; further > 0 {
			out += fmt.Sprintf("… %d further matches\n", further)
		}
	} else {
		if tail > 0 && len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		out = strings.Join(lines, "\n") + "\n"
		printed = len(lines)
	}
	return logOut{Schema: api.SchemaV1, Artifact: target, Lines: printed, Text: out}
}

// grepLog selects the lines matching re, expands each by context lines on either
// side, merges overlapping windows so no line prints twice, and numbers what it
// keeps. limit caps the matches considered (0 means no limit); kept is how many
// of total matches survived that cap, so the caller can say what it dropped.
func grepLog(lines []string, re *regexp.Regexp, context, limit int) (out []string, total, kept int) {
	var matches []int
	for i, l := range lines {
		if re.MatchString(l) {
			matches = append(matches, i)
		}
	}
	total = len(matches)
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	kept = len(matches)

	var keep []int
	for _, i := range matches {
		lo, hi := i-context, i+context
		if lo < 0 {
			lo = 0
		}
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		if n := len(keep); n > 0 && lo <= keep[n-1]+1 {
			// Overlapping or adjacent window: extend it rather than repeat it.
			for j := keep[n-1] + 1; j <= hi; j++ {
				keep = append(keep, j)
			}
			continue
		}
		for j := lo; j <= hi; j++ {
			keep = append(keep, j)
		}
	}
	out = make([]string, len(keep))
	for i, n := range keep {
		out[i] = fmt.Sprintf("%5d  %s", n+1, lines[n])
	}
	return out, total, kept
}

// maxLogBytes caps how much of a build log is read, so a huge log cannot fill
// memory for the sake of a 120-line tail.
const maxLogBytes = 8 << 20

// resolveStepLog reads a step's build log and returns the object it came from.
// It fetches the paths Prow actually writes before it considers listing the
// run, because a full listing of a large job is tens of megabytes of metadata
// for one small object:
//
//	<run>/artifacts/<step>/build-log.txt            ci-operator steps
//	<run>/artifacts/<test-name>/<step>/build-log.txt  most step logs
//
// The whole-run scan is kept as a last resort for layouts that nest deeper.
func resolveStepLog(ctx context.Context, c *gcs.Client, bucket, runPath, step string) (string, []byte, error) {
	if step == "job" {
		if name, data, err := getLog(ctx, c, bucket, runPath+"/build-log.txt"); !errors.Is(err, gcs.ErrNotFound) {
			return name, data, err
		}
		return "", nil, noStepLog(step)
	}

	if name, data, err := getLog(ctx, c, bucket, runPath+"/artifacts/"+step+"/build-log.txt"); !errors.Is(err, gcs.ErrNotFound) {
		return name, data, err
	}

	// One level deeper, under the operator test's own directory.
	dirs, err := c.ListDirs(ctx, bucket, runPath+"/artifacts/")
	if err != nil {
		return "", nil, err
	}
	for _, dir := range dirs {
		name, data, perr := getLog(ctx, c, bucket, dir+step+"/build-log.txt")
		if errors.Is(perr, gcs.ErrNotFound) {
			continue
		}
		return name, data, perr
	}

	objs, err := c.List(ctx, bucket, runPath+"/")
	if err != nil {
		return "", nil, err
	}
	for _, o := range objs {
		if strings.Contains(o.Name, "/"+step+"/build-log.txt") {
			return getLog(ctx, c, bucket, o.Name)
		}
	}
	return "", nil, noStepLog(step)
}

// getLog fetches one candidate path, passing ErrNotFound through so the caller
// can try the next layout. Logs are one-shot and large, so they are never
// cached.
func getLog(ctx context.Context, c *gcs.Client, bucket, name string) (string, []byte, error) {
	data, err := c.GetLimit(ctx, bucket, name, maxLogBytes)
	if err != nil {
		return "", nil, err
	}
	return name, data, nil
}

func noStepLog(step string) error {
	return fmt.Errorf("no build-log.txt found for step %q", step)
}

// ---------- artifact ----------

func newArtifactCmd(g *globals) *cobra.Command {
	var list bool
	var limit int
	cmd := &cobra.Command{
		Use:   "artifact <gcs-url | gs://bucket/object>",
		Short: "Metadata and a bounded excerpt for a GCS artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			bucket, object, err := prow.ParseArtifactURL(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()

			if list || strings.HasSuffix(object, "/") {
				objs, truncated, err := listArtifacts(ctx, c, bucket, object, limit)
				if err != nil {
					return err
				}
				entries := make([]render.ArtifactEntry, len(objs))
				var total int64
				for i, o := range objs {
					entries[i] = render.ArtifactEntry{Name: o.Name, Size: o.Size}
					total += o.Size
				}
				var buf bytes.Buffer
				// Both forms are built from the same entries and total, so the
				// table and the document cannot disagree. A prefix with nothing
				// under it is an empty list, not an error.
				if g.jsonOut {
					writeJSON(&buf, artifactListOut{
						Schema:     api.SchemaV1,
						Prefix:     object,
						Objects:    entries,
						TotalBytes: total,
						Truncated:  truncated,
					})
				} else {
					render.ArtifactList(&buf, g.style(), entries, truncated)
				}
				g.emit(&buf, stats.Record{Cmd: "artifact-list", Target: object, InventoryBytes: total}, c, start)
				return nil
			}

			meta, statErr := c.Stat(ctx, bucket, object)
			if statErr != nil && statErr != gcs.ErrNotFound {
				return statErr
			}

			isLog := path.Base(object) == "build-log.txt"
			const maxExcerpt = 64 << 10
			data, err := c.GetLimit(ctx, bucket, object, maxExcerpt)
			if err != nil {
				return err
			}

			var buf bytes.Buffer
			if g.jsonOut {
				excerpt := string(data)
				if isLog {
					lines := strings.Split(strings.TrimRight(excerpt, "\n"), "\n")
					if len(lines) > 120 {
						lines = lines[len(lines)-120:]
					}
					excerpt = strings.Join(lines, "\n")
				}
				writeJSON(&buf, artifactOut{
					Schema:      api.SchemaV1,
					Object:      object,
					Size:        meta.Size,
					ContentType: meta.ContentType,
					Updated:     meta.Updated,
					Excerpt:     excerpt,
					Truncated:   meta.Size > int64(len(data)),
				})
			} else {
				render.Artifact(&buf, g.style(), render.ArtifactInfo{
					Name:        object,
					Size:        meta.Size,
					ContentType: meta.ContentType,
					Updated:     meta.Updated,
					Data:        data,
					IsLog:       isLog,
					Truncated:   meta.Size > int64(len(data)),
				})
			}
			g.emit(&buf, stats.Record{Cmd: "artifact", Target: object, InventoryBytes: meta.Size}, c, start)
			return nil
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "list a prefix instead of reading an object")
	cmd.Flags().IntVar(&limit, "limit", 200, "maximum objects to list; 0 = all")
	return cmd
}

// listArtifacts returns at most limit objects under prefix. It asks for one
// extra so a complete listing is distinguishable from a truncated one, which
// ListLimit alone cannot report; limit 0 means every object.
func listArtifacts(ctx context.Context, c *gcs.Client, bucket, prefix string, limit int) ([]gcs.Object, bool, error) {
	if limit <= 0 {
		objs, err := c.List(ctx, bucket, prefix)
		return objs, false, err
	}
	objs, err := c.ListLimit(ctx, bucket, prefix, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(objs) > limit {
		return objs[:limit], true, nil
	}
	return objs, false, nil
}

// ---------- search ----------

func newSearchCmd(g *globals) *cobra.Command {
	var maxAge string
	cmd := &cobra.Command{
		Use:   "search <regex>",
		Short: "Count matching runs on search.ci",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()
			res, err := enrich.SearchCI(ctx, c.HTTP, args[0], maxAge)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, res)
			} else {
				render.Search(&buf, g.style(), res.Matches, res.Jobs, res.KnownBug, maxAge)
			}
			g.emit(&buf, stats.Record{Cmd: "search", Target: args[0]}, c, start)
			return nil
		},
	}
	cmd.Flags().StringVar(&maxAge, "max-age", "48h", "search window")
	return cmd
}

// ---------- gain ----------

func newGainCmd(g *globals) *cobra.Command {
	var since string
	var by string
	var history bool
	var fun bool
	cmd := &cobra.Command{
		Use:     "gain",
		Aliases: []string{"stats", "saved"},
		Short:   "Savings statistics (bytes, tokens, time)",
		RunE: func(cmd *cobra.Command, args []string) error {
			recs, err := stats.Load()
			if err != nil {
				return err
			}
			var cutoff time.Time
			if since != "" {
				d, err := parseSince(since)
				if err != nil {
					return err
				}
				cutoff = time.Now().Add(-d)
			}
			agg := stats.Summarize(recs, by, cutoff)
			if fun {
				recs = stats.InWindow(recs, cutoff)
			}
			var buf bytes.Buffer
			if history {
				start := len(recs) - 20
				if start < 0 {
					start = 0
				}
				render.History(&buf, g.style(), recs[start:])
				_, _ = os.Stdout.Write(buf.Bytes())
				return nil
			}
			if g.jsonOut {
				writeJSON(&buf, agg)
			} else {
				render.Gain(&buf, agg, g.bytesPerToken, g.baseline, g.style())
				if fun {
					render.Fun(&buf, g.style(), recs)
				}
			}
			_, _ = os.Stdout.Write(buf.Bytes())
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only count calls newer than this (e.g. 7d, 24h)")
	cmd.Flags().StringVar(&by, "by", "", "group by: cmd or repo")
	cmd.Flags().BoolVar(&history, "history", false, "show recent calls")
	cmd.Flags().BoolVar(&fun, "fun", false, "extra, less serious statistics")
	return cmd
}

func parseSince(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("invalid --since %q", s)
}

// ---------- hook ----------

func newHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook <bash|webfetch>",
		Short:  "Claude Code PreToolUse hook entrypoint",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return hook.Handle(args[0], os.Stdin, os.Stdout)
		},
	}
}

// ---------- doctor ----------

func newDoctorCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check binary, cache, stats, and endpoint reachability",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient(g)
			st := g.style()
			cacheBytes, cacheFiles, _ := c.CacheSize()
			home, _ := os.UserHomeDir()
			cwd, _ := os.Getwd()
			// The MCP row is a file read in both scopes, never `claude mcp get`,
			// so doctor stays offline and fast.
			doctorLocal(os.Stdout, st, c.CacheDir, cacheBytes, cacheFiles, claudecfg.MCPRegisteredScope(cwd, home))
			doctorClaude(os.Stdout, st, filepath.Join(claudecfg.ConfigDir(home), "settings.json"))

			// The probes write as they finish rather than buffering the card, so
			// a slow endpoint cannot leave the terminal blank.
			ctx1, cancel1 := context.WithTimeout(context.Background(), 8*time.Second)
			if _, err := c.ListLimit(ctx1, g.bucket, "pr-logs/directory/", 5); err == nil {
				doctorLine(os.Stdout, st, "gcs", st.Green("ok"))
			} else {
				doctorLine(os.Stdout, st, "gcs", st.Red("FAILED: "+err.Error()))
			}
			cancel1()

			ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
			if sippy := enrich.SippyStatus(ctx2, c.HTTP, "0"); sippy == "unknown" {
				doctorLine(os.Stdout, st, "sippy", st.Yellow("unreachable or unexpected response"))
			} else {
				doctorLine(os.Stdout, st, "sippy", st.Green("ok ("+sippy+")"))
			}
			cancel2()

			ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := enrich.SearchCI(ctx3, c.HTTP, "whyfail-doctor-probe", "1h"); err == nil {
				doctorLine(os.Stdout, st, "search.ci", st.Green("ok"))
			} else {
				doctorLine(os.Stdout, st, "search.ci", st.Red("FAILED: "+err.Error()))
			}
			cancel3()
			return nil
		},
	}
}

// doctorLine writes one dim-labelled prerequisite row, aligned with the gutter
// the rest of the tool uses.
func doctorLine(w io.Writer, st term.Style, label, value string) {
	fmt.Fprintf(w, "%s %s\n", st.Dim(term.PadRight(label+":", 10)), value)
}

// doctorLocal writes the prerequisite checks that need no network. It takes a
// writer so the report can be tested without a live bucket. Nothing here is
// recorded in stats: doctor reads no artifacts, so counting it would inflate
// `gain` with calls that saved nothing.
func doctorLocal(w io.Writer, st term.Style, cacheDir string, cacheBytes int64, cacheFiles int, mcpScope claudecfg.Scope) {
	doctorLine(w, st, "version", version)
	cacheValue := cacheDir
	if cacheFiles > 0 {
		cacheValue = fmt.Sprintf("%s (%s, %d objects)", cacheDir, render.FormatBytes(cacheBytes), cacheFiles)
	}
	doctorLine(w, st, "cache", cacheValue)
	doctorLine(w, st, "stats", fmt.Sprintf("%s (enabled=%v)", stats.Path(), stats.Enabled()))
	doctorLine(w, st, "binary", binaryStatus())
	doctorLine(w, st, "gh", ghStatus())
	// ScopeNone means neither file names the server, which is the state a client
	// with no tools in it is usually in.
	mcpValue := "not registered (run: whyfail init --mcp)"
	if mcpScope != claudecfg.ScopeNone {
		mcpValue = "registered (" + string(mcpScope) + ")"
	}
	doctorLine(w, st, "mcp", mcpValue)
}

// binaryStatus names the whyfail on PATH and calls out when it is not the
// binary being run — the usual surprise after a rebuild.
func binaryStatus() string {
	onPath, err := exec.LookPath("whyfail")
	if err != nil {
		return "not on PATH (hooks will fail)"
	}
	self, err := os.Executable()
	if err != nil || sameFile(self, onPath) {
		return onPath
	}
	return onPath + " (running " + self + ", possibly stale)"
}

// ghStatus reports the CLI the pr command needs, rather than letting pr fail
// after it has been typed.
func ghStatus() string {
	p, err := exec.LookPath("gh")
	if err != nil {
		return "missing (whyfail pr will not work)"
	}
	return p
}

// sameFile compares two paths after resolving symlinks, so a linked install is
// not reported as stale.
func sameFile(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ra == rb
}

// ---------- pr ----------
var prURLRe = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)/?$`)

// prShorthandRe matches owner/repo#N, the form a PR is usually referred to in
// review and issue comments.
var prShorthandRe = regexp.MustCompile(`^([^/\s#]+)/([^/\s#]+)#(\d+)$`)

// prNumberRe matches a bare PR number, which needs --repo to be unambiguous.
var prNumberRe = regexp.MustCompile(`^(\d+)$`)

// parsePRArg resolves the accepted PR arguments into org, repo and number.
// repoFlag supplies the repository for a bare number; it is never inferred from
// the working directory or gh, so a typo stays an error rather than a lookup.
func parsePRArg(arg, repoFlag string) (org, repo, num string, err error) {
	if m := prURLRe.FindStringSubmatch(arg); m != nil {
		return m[1], m[2], m[3], nil
	}
	if m := prShorthandRe.FindStringSubmatch(arg); m != nil {
		return m[1], m[2], m[3], nil
	}
	if m := prNumberRe.FindStringSubmatch(arg); m != nil {
		if repoFlag == "" {
			return "", "", "", fmt.Errorf("%s is a bare PR number; pass --repo owner/repo", arg)
		}
		o, r, ok := strings.Cut(repoFlag, "/")
		if !ok || o == "" || r == "" || strings.Contains(r, "/") {
			return "", "", "", fmt.Errorf("--repo must be owner/repo, got %q", repoFlag)
		}
		return o, r, m[1], nil
	}
	return "", "", "", fmt.Errorf("not a GitHub PR reference: %s (want a URL, owner/repo#N, or N with --repo)", arg)
}

// canonicalPRURL is the one form the rest of the command and the JSON document
// use, whatever the input spelling.
func canonicalPRURL(org, repo, num string) string {
	return "https://github.com/" + org + "/" + repo + "/pull/" + num
}

// prFanOut is how many failing checks are looked up at once. Each health.Collect
// fans out internally, so the outer width stays small.
const prFanOut = 4

func newPRCmd(g *globals) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "pr <github-pr-url | owner/repo#N | N>",
		Short: "Health of every failing check on a PR's head commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			org, repo, num, err := parsePRArg(args[0], repoFlag)
			if err != nil {
				return err
			}
			if _, err := exec.LookPath("gh"); err != nil {
				return fmt.Errorf("the pr command needs the gh CLI for GitHub access")
			}
			ctx, cancel := g.ctx()
			defer cancel()
			defer func() { err = g.timeoutError(cmd.CommandPath(), err) }()
			c := newClient(g)
			start := time.Now()

			doc, err := buildPRReport(ctx, g, c, execGH, canonicalPRURL(org, repo, num))
			if err != nil {
				return err
			}

			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, doc)
			} else {
				render.PR(&buf, g.style(), org, repo, num, doc.Title, doc.Failing, doc.Warning)
			}
			g.emit(&buf, stats.Record{Cmd: "pr", Target: org + "/" + repo + "#" + num, Repo: org + "/" + repo}, c, start)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "owner/repo, for a bare PR number")
	return cmd
}

// buildPRReport resolves a PR and collects the health of every failing check.
// It takes the gh seam rather than calling execGH, so the CLI and the MCP server
// share one path and tests can drive it without a real gh.
func buildPRReport(ctx context.Context, g *globals, c *gcs.Client, gh ghAPI, prURL string) (*prOut, error) {
	m := prURLRe.FindStringSubmatch(prURL)
	if m == nil {
		return nil, fmt.Errorf("not a GitHub PR URL: %s", prURL)
	}
	org, repo, num := m[1], m[2], m[3]

	raw, err := gh(ctx, "repos/"+org+"/"+repo+"/pulls/"+num, false)
	if err != nil {
		return nil, err
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("decode pull request: %w", err)
	}
	sha := stringField(meta, "head", "sha")
	base := stringField(meta, "base", "ref")
	title, _ := meta["title"].(string)
	if sha == "" {
		return nil, fmt.Errorf("could not read head SHA for %s", prURL)
	}

	cards, warning, err := collectPRChecks(ctx, gh, c, g.bucket, org, repo, base, sha, g.window)
	if err != nil {
		return nil, err
	}
	return &prOut{
		Schema:  api.SchemaV1,
		PR:      prURL,
		Title:   title,
		SHA:     sha,
		Failing: cards,
		Warning: warning,
	}, nil
}

// commitStatus is one entry from the legacy commit-status API, which Prow uses.
type commitStatus struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

// checkRun is one entry from the Checks API, which GitHub Actions uses. The API
// identifies a run by (app, name, head_sha, check_suite), not by name alone: two
// workflows can emit the same job name, so App is part of the run's identity.
type checkRun struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion *string   `json:"conclusion"`
	App        *checkApp `json:"app"`
}

// checkApp is the app that owns a check run. It is null on a run left behind by
// an app that has since been removed, so it is a pointer.
type checkApp struct {
	Slug string `json:"slug"`
}

// checkRunsPage is the Checks API envelope. Unlike the statuses endpoint, which
// returns a bare array, this returns an object wrapping the runs.
type checkRunsPage struct {
	CheckRuns []checkRun `json:"check_runs"`
}

// failingCheckRun reports whether a completed check run failed. A check that is
// still running has a null conclusion and is not a failure yet.
func failingCheckRun(r checkRun) bool {
	if r.Status != "completed" || r.Conclusion == nil {
		return false
	}
	switch *r.Conclusion {
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
		return true
	}
	return false
}

// collectPRChecks reads every failing check on a head commit. Prow reports
// through the legacy commit-status API and Actions through the Checks API, so
// both are needed: reading statuses alone hides every Actions failure, and the
// command then claims the PR has none.
func collectPRChecks(ctx context.Context, api ghAPI, c *gcs.Client, bucket, org, repo, branch, sha string, window time.Duration) ([]render.PRCard, string, error) {
	base := "repos/" + org + "/" + repo + "/commits/" + sha

	statusRaw, err := api(ctx, base+"/statuses?per_page=100", true)
	if err != nil {
		return nil, "", err
	}
	statuses, err := decodePages[commitStatus](statusRaw)
	if err != nil {
		return nil, "", fmt.Errorf("decode commit statuses: %w", err)
	}
	runsRaw, err := api(ctx, base+"/check-runs?per_page=100&filter=latest", true)
	if err != nil {
		return nil, "", err
	}
	runs, err := decodeCheckRunPages(runsRaw)
	if err != nil {
		return nil, "", fmt.Errorf("decode check runs: %w", err)
	}

	var cards []render.PRCard
	// A card is keyed by the API it came from as well as the name it carries:
	// two workflows can emit the same job name, and collapsing them by name
	// silently dropped one of them from a list meant to be exhaustive.
	seen := map[string]bool{}
	// A status claims its name outright: Prow's context is what job-name
	// synthesis understands, so it beats a check run that reuses the name.
	claimed := map[string]bool{}
	// Statuses first: the API returns the newest entry per context first.
	for _, s := range statuses {
		key := render.SourceStatus + "|" + s.Context
		if seen[key] {
			continue
		}
		seen[key] = true
		claimed[s.Context] = true
		if s.State != "failure" && s.State != "error" {
			continue
		}
		test := strings.TrimPrefix(s.Context, "ci/prow/")
		if test == s.Context || test == "" {
			// A failing check with no Prow job behind it: report it rather than
			// dropping it, which is what made an all-Actions PR look clean.
			cards = append(cards, render.PRCard{Check: s.Context, Source: render.SourceStatus})
			continue
		}
		cards = append(cards, render.PRCard{
			Check:  s.Context,
			Job:    "pull-ci-" + org + "-" + repo + "-" + branch + "-" + test,
			Source: render.SourceStatus,
			Prow:   true,
		})
	}
	for _, r := range runs {
		app := ""
		if r.App != nil {
			app = r.App.Slug
		}
		if claimed[r.Name] || !failingCheckRun(r) {
			continue
		}
		// An empty slug is its own bucket rather than a wildcard: a run from a
		// vanished app must not absorb one from a live app.
		key := render.SourceCheckRun + "|" + r.Name + "|" + app
		if seen[key] {
			continue
		}
		seen[key] = true
		cards = append(cards, render.PRCard{Check: r.Name, App: app, Source: render.SourceCheckRun})
	}

	// Each Collect already fans out internally, so keep the outer width small.
	// Only Prow checks have a job to look up. A lane's panic is reported by Do
	// but does not fail the command: one check's health is optional.
	//
	// A lookup that fails is recorded on its own card rather than folded into
	// "no history": a 403, a timeout and an absent job are three different
	// facts, and only the last one belongs to the job.
	warning := ""
	if derr := parallel.Do(len(cards), prFanOut, func(i int) error {
		if !cards[i].Prow {
			return nil
		}
		sum, err := health.Collect(ctx, c, bucket, cards[i].Job, window)
		if err != nil {
			if !errors.Is(err, health.ErrJobNotFound) {
				cards[i].HealthErr = err.Error()
			}
			return nil
		}
		cards[i].Health = sum
		return nil
	}); derr != nil {
		warning = derr.Error()
	}
	return cards, warning, nil
}

// decodePages decodes one or more concatenated JSON arrays. `gh api --paginate`
// writes each page as a separate array with no separator between them, so a
// single Unmarshal would fail on the second page.
func decodePages[T any](b []byte) ([]T, error) {
	var out []T
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var page []T
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
}

// decodeCheckRunPages decodes the concatenated envelopes `gh api --paginate`
// writes for the Checks API.
func decodeCheckRunPages(b []byte) ([]checkRun, error) {
	var out []checkRun
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var page checkRunsPage
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, page.CheckRuns...)
	}
}

// ghAPI is one `gh api` request. paginate follows the Link header and returns
// every page's JSON array concatenated, which is what the checks endpoints need.
// It takes the command's context so a hung `gh` is killed by --timeout like
// every in-process request.
type ghAPI func(ctx context.Context, endpoint string, paginate bool) ([]byte, error)

// execGH runs `gh api`. Its signature satisfies ghAPI, so callers that want to
// be testable take a ghAPI and tests pass a canned one.
func execGH(ctx context.Context, endpoint string, paginate bool) ([]byte, error) {
	args := []string{"api"}
	if paginate {
		args = append(args, "--paginate")
	}
	args = append(args, endpoint)
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		// A process killed on the deadline reports "signal: killed", which would
		// hide the reason. Report the context's own error instead, so
		// errors.Is(err, context.DeadlineExceeded) still holds.
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
		}
		return nil, fmt.Errorf("gh api %s: %w", endpoint, err)
	}
	return out, nil
}

func ghGet(ctx context.Context, endpoint string) ([]byte, error) {
	return execGH(ctx, endpoint, false)
}

func ghJSON(ctx context.Context, endpoint string) (map[string]any, error) {
	out, err := ghGet(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func stringField(m map[string]any, obj, field string) string {
	inner, _ := m[obj].(map[string]any)
	v, _ := inner[field].(string)
	return v
}

// ---------- mcp ----------

func newMCPCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Run a minimal MCP stdio server",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveMCP(g)
		},
	}
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// mcpTools is the tool catalogue, named so a test can assert the list a client
// sees. Keep each inputSchema in step with the arguments mcpCallWith reads.
var mcpTools = []map[string]any{
	{"name": "prow_run", "description": "Digest of a Prow job run", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []string{"url"}}},
	{"name": "prow_job", "description": "Health of a Prow job over retained history", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"job": map[string]any{"type": "string"}}, "required": []string{"job"}}},
	{"name": "prow_pr", "description": "Health of every failing check on a pull request's head commit", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"pr": map[string]any{"type": "string"}}, "required": []string{"pr"}}},
	{"name": "prow_search", "description": "Count matching runs on search.ci for a regular expression", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "max_age": map[string]any{"type": "string", "description": "search window, default 48h"}}, "required": []string{"query"}}},
	{"name": "prow_log", "description": "Bounded view of a step's build log", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}, "step": map[string]any{"type": "string"}, "tail": map[string]any{"type": "integer", "description": "trailing lines or matches, default 120"}, "grep": map[string]any{"type": "string"}}, "required": []string{"url", "step"}}},
}

func serveMCP(g *globals) error {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	enc := json.NewEncoder(os.Stdout)

	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req rpcReq
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "whyfail", "version": version},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": mcpTools}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if req.ID == nil {
				// A notification asks for no response, so running the call would
				// spend the work and record a use nobody can read the result of.
				continue
			}
			text, err := mcpCall(g, p.Name, p.Arguments)
			if err != nil {
				resp["error"] = map[string]any{"code": -32000, "message": err.Error()}
			} else {
				resp["result"] = map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				}
			}
		default:
			if req.ID == nil {
				continue // notification
			}
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if req.ID == nil && req.Method != "initialize" {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func mcpCall(g *globals, name string, args map[string]any) (string, error) {
	// The bound is per tool call, not around serveMCP: the server is long-lived.
	ctx, cancel := g.ctx()
	defer cancel()
	return mcpCallWith(ctx, g, newClient(g), name, args)
}

// mcpCallWith is mcpCall with the context and client supplied, so a test can
// drive a tool against the recorded corpus instead of the public bucket.
func mcpCallWith(ctx context.Context, g *globals, c *gcs.Client, name string, args map[string]any) (text string, err error) {
	start := time.Now()
	// An agent looping over failing checks is the highest-volume path through
	// the tool, so every call is recorded like a CLI command. The record goes to
	// the stats file, never to stdout, which is the JSON-RPC transport here. A
	// failed call is recorded too: a retry loop against a bad URL is usage, and
	// leaving it out would flatter the reported ratio.
	defer func() {
		rec := stats.Record{Cmd: "mcp/" + name, Target: mcpTarget(name, args), Repo: mcpRepo(name, args)}
		if err != nil {
			rec.EmittedBytes = int64(len(err.Error()))
		} else {
			rec.EmittedBytes = int64(len(text))
		}
		g.record(rec, c, start)
	}()
	// Registered after the record so it runs first: the recorded error is then
	// the one the caller sees, not a bare context deadline.
	defer func() { err = g.timeoutError("tool "+name, err) }()
	switch name {
	case "prow_run":
		raw, _ := args["url"].(string)
		ref, err := prow.ParseRef(raw)
		if err != nil {
			return "", err
		}
		d, err := run.Build(ctx, c, ref, run.Options{History: true, Window: g.window})
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(d)
		return string(b), nil
	case "prow_job":
		job, _ := args["job"].(string)
		sum, err := health.Collect(ctx, c, g.bucket, job, g.window)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(sum)
		return string(b), nil
	case "prow_pr":
		prURL, _ := args["pr"].(string)
		doc, err := buildPRReport(ctx, g, c, execGH, prURL)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(doc)
		return string(b), nil
	case "prow_search":
		query, _ := args["query"].(string)
		if query == "" {
			return "", fmt.Errorf("prow_search needs a non-empty query")
		}
		maxAge, _ := args["max_age"].(string)
		res, err := enrich.SearchCI(ctx, c.HTTP, query, maxAge)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(res)
		return string(b), nil
	case "prow_log":
		raw, _ := args["url"].(string)
		step, _ := args["step"].(string)
		ref, err := prow.ParseRef(raw)
		if err != nil {
			return "", err
		}
		if step == "" {
			return "", fmt.Errorf("prow_log needs a step")
		}
		grep, _ := args["grep"].(string)
		var re *regexp.Regexp
		if grep != "" {
			re, err = regexp.Compile(grep)
			if err != nil {
				return "", fmt.Errorf("invalid grep pattern: %w", err)
			}
		}
		target, data, err := resolveStepLog(ctx, c, ref.Bucket, ref.ObjectPath, step)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(logDocument(target, data, intArg(args, "tail", 120), re, 0))
		return string(b), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// intArg reads an optional integer argument, which JSON-RPC decodes as a float,
// falling back to def when it is absent or not positive.
func intArg(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}

// mcpTarget names what a tool call was aimed at, so its record reads without
// the arguments: the run URL for prow_run, the job name for prow_job.
// mcpRepo names the repository a tool call read, using the same derivation as
// the CLI commands. A job name does not identify a repository — both org and
// repo may contain dashes — so prow_job records none rather than guessing.
func mcpRepo(name string, args map[string]any) string {
	if name == "prow_pr" {
		s, _ := args["pr"].(string)
		if m := prURLRe.FindStringSubmatch(s); m != nil {
			return m[1] + "/" + m[2]
		}
		return ""
	}
	if name != "prow_run" && name != "prow_log" {
		return ""
	}
	s, _ := args["url"].(string)
	ref, err := prow.ParseRef(s)
	if err != nil {
		return ""
	}
	return prow.Repo(ref.ObjectPath)
}

func mcpTarget(name string, args map[string]any) string {
	switch name {
	case "prow_run":
		s, _ := args["url"].(string)
		return s
	case "prow_job":
		s, _ := args["job"].(string)
		return s
	case "prow_pr":
		s, _ := args["pr"].(string)
		return s
	case "prow_search":
		s, _ := args["query"].(string)
		return s
	case "prow_log":
		s, _ := args["url"].(string)
		return s
	}
	return ""
}
