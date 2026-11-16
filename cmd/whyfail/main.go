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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
}

func main() {
	g := &globals{bytesPerToken: 4, baseline: 10 * time.Minute, colorMode: "auto", retries: 2}
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

	root.AddCommand(
		newRunCmd(g), newJobCmd(g), newFailuresCmd(g), newLogCmd(g), newArtifactCmd(g),
		newSearchCmd(g), newGainCmd(g), newHookCmd(), newDoctorCmd(g), newPRCmd(g), newMCPCmd(g),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "whyfail:", err)
		os.Exit(1)
	}
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
	return c
}

// emit writes the rendered output and records one stats entry.
func (g *globals) emit(buf *bytes.Buffer, rec stats.Record, c *gcs.Client, start time.Time) {
	_, _ = os.Stdout.Write(buf.Bytes())
	if g.noStats || !stats.Enabled() {
		return
	}
	rec.TS = time.Now().UTC()
	// Count visible bytes only, so escape sequences never inflate the savings.
	rec.EmittedBytes = int64(term.PlainBytes(buf.String()))
	if c != nil {
		rec.ReadBytes = c.ServedBytes
		rec.NetworkBytes = c.NetworkBytes
		rec.CacheHits = c.CacheHits
		rec.Missing = c.Missing
	}
	rec.WallMS = time.Since(start).Milliseconds()
	_ = stats.Append(rec)
}

func writeJSON(buf *bytes.Buffer, v any) {
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ---------- run ----------

func newRunCmd(g *globals) *cobra.Command {
	var noHistory, search bool
	var noSippy bool
	cmd := &cobra.Command{
		Use:   "run <prow-url | gs://bucket/path>",
		Short: "Digest a single Prow job run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := prow.ParseRef(args[0])
			if err != nil {
				return err
			}
			ctx := context.Background()
			c := newClient(g)
			start := time.Now()

			d, err := run.Build(ctx, c, ref, run.Options{History: !noHistory})
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
			}
			g.emit(&buf, rec, c, start)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noHistory, "no-history", false, "skip job history (faster)")
	cmd.Flags().BoolVar(&search, "search", false, "look up the top failure signature on search.ci")
	cmd.Flags().BoolVar(&noSippy, "no-sippy", false, "skip the Sippy coverage probe")
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
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
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
			g.emit(&buf, stats.Record{Cmd: "job", Target: args[0], Verdict: string(sum.Verdict)}, c, start)
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
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := prow.ParseRef(args[0])
			if err != nil {
				return err
			}
			ctx := context.Background()
			c := newClient(g)
			start := time.Now()
			d, err := run.Build(ctx, c, ref, run.Options{History: false})
			if err != nil {
				return err
			}
			out := struct {
				Run    *run.RunInfo  `json:"run"`
				Tests  *run.Tests    `json:"tests,omitempty"`
				Failed []run.StepRef `json:"failed_steps,omitempty"`
			}{Run: d.Run, Tests: d.Tests, Failed: d.Run.Steps.Failed}

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
	cmd := &cobra.Command{
		Use:   "log <prow-url> <step> [--tail N]",
		Short: "Bounded view of a step's build log",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := prow.ParseRef(args[0])
			if err != nil {
				return err
			}
			step := args[1]
			ctx := context.Background()
			c := newClient(g)
			start := time.Now()

			target, data, err := resolveStepLog(ctx, c, ref.Bucket, ref.ObjectPath, step)
			if err != nil {
				return err
			}
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if tail > 0 && len(lines) > tail {
				lines = lines[len(lines)-tail:]
			}
			out := strings.Join(lines, "\n") + "\n"

			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, map[string]any{"artifact": target, "lines": len(lines), "text": out})
			} else {
				buf.WriteString(out)
			}
			g.emit(&buf, stats.Record{Cmd: "log", Target: target, Repo: prow.Repo(ref.ObjectPath)}, c, start)
			return nil
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 120, "number of trailing lines")
	return cmd
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
		RunE: func(cmd *cobra.Command, args []string) error {
			bucket, object, err := prow.ParseArtifactURL(args[0])
			if err != nil {
				return err
			}
			ctx := context.Background()
			c := newClient(g)
			start := time.Now()

			if list || strings.HasSuffix(object, "/") {
				objs, truncated, err := listArtifacts(ctx, c, bucket, object, limit)
				if err != nil {
					return err
				}
				var buf bytes.Buffer
				st := g.style()
				var total int64
				for _, o := range objs {
					total += o.Size
					fmt.Fprintf(&buf, "  %s  %s\n",
						st.Dim(term.PadLeft(render.FormatBytes(o.Size), 9)),
						term.TruncMiddle(o.Name, st.Width()-14))
				}
				if truncated {
					// ListLimit reads one page, so the true total is unknown and
					// "at least" is the honest wording.
					fmt.Fprintf(&buf, "  %s\n", st.Dim(fmt.Sprintf(
						"%d of at least %d objects, %s shown · use --limit 0 for all",
						len(objs), len(objs)+1, render.FormatBytes(total))))
				} else {
					fmt.Fprintf(&buf, "  %s\n", st.Dim(fmt.Sprintf("%d objects, %s total",
						len(objs), render.FormatBytes(total))))
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
			metaOut := map[string]any{
				"object":       object,
				"size":         meta.Size,
				"content_type": meta.ContentType,
				"updated":      meta.Updated,
			}
			if g.jsonOut {
				excerpt := string(data)
				if isLog {
					lines := strings.Split(strings.TrimRight(excerpt, "\n"), "\n")
					if len(lines) > 120 {
						lines = lines[len(lines)-120:]
					}
					excerpt = strings.Join(lines, "\n")
				}
				metaOut["excerpt"] = excerpt
				metaOut["truncated"] = meta.Size > int64(len(data))
				writeJSON(&buf, metaOut)
			} else {
				fmt.Fprintf(&buf, "%s\n  size: %s\n  type: %s\n  updated: %s\n", object,
					render.FormatBytes(meta.Size), meta.ContentType, meta.Updated)
				if isLog {
					lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
					if len(lines) > 120 {
						lines = lines[len(lines)-120:]
					}
					buf.WriteString("\n--- tail ---\n")
					buf.WriteString(strings.Join(lines, "\n"))
					buf.WriteString("\n")
				} else if len(data) > 0 && isText(data) {
					buf.WriteString("\n--- excerpt ---\n")
					buf.Write(data)
					if meta.Size > int64(len(data)) {
						fmt.Fprintf(&buf, "\n... (%s truncated at %s)\n", render.FormatBytes(meta.Size), render.FormatBytes(maxExcerpt))
					}
				}
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

func isText(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// ---------- search ----------

func newSearchCmd(g *globals) *cobra.Command {
	var maxAge string
	cmd := &cobra.Command{
		Use:   "search <regex>",
		Short: "Count matching runs on search.ci",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
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
				fmt.Fprintf(&buf, "%d matching jobs in the last %s\n", res.Matches, maxAge)
				for i, j := range res.Jobs {
					if i >= 10 {
						fmt.Fprintf(&buf, "  ... and %d more\n", len(res.Jobs)-10)
						break
					}
					fmt.Fprintf(&buf, "  %s\n", j)
				}
				if res.KnownBug != "" {
					fmt.Fprintf(&buf, "known bug referenced: %s\n", res.KnownBug)
				}
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
			var buf bytes.Buffer
			if history {
				start := len(recs) - 20
				if start < 0 {
					start = 0
				}
				for _, r := range recs[start:] {
					fmt.Fprintf(&buf, "%s  %-8s %10s → %8s  %s\n",
						r.TS.Format("2006-01-02 15:04"), r.Cmd,
						render.FormatBytes(r.ReadBytes), render.FormatBytes(r.EmittedBytes), r.Target)
				}
				_, _ = os.Stdout.Write(buf.Bytes())
				return nil
			}
			if g.jsonOut {
				writeJSON(&buf, agg)
			} else {
				render.Gain(&buf, agg, g.bytesPerToken, g.baseline, g.style())
			}
			_, _ = os.Stdout.Write(buf.Bytes())
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only count calls newer than this (e.g. 7d, 24h)")
	cmd.Flags().StringVar(&by, "by", "", "group by: cmd or repo")
	cmd.Flags().BoolVar(&history, "history", false, "show recent calls")
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

			fmt.Printf("version:   %s\n", version)
			fmt.Printf("cache:     %s\n", c.CacheDir)
			fmt.Printf("stats:     %s (enabled=%v)\n", stats.Path(), stats.Enabled())
			if p, err := exec.LookPath("whyfail"); err == nil {
				fmt.Printf("binary:    %s\n", p)
			} else {
				fmt.Println("binary:    not on PATH (hooks will fail)")
			}

			ctx1, cancel1 := context.WithTimeout(context.Background(), 8*time.Second)
			if _, err := c.ListLimit(ctx1, g.bucket, "pr-logs/directory/", 5); err == nil {
				fmt.Println("gcs:       ok")
			} else {
				fmt.Printf("gcs:       FAILED: %v\n", err)
			}
			cancel1()

			ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
			if s := enrich.SippyStatus(ctx2, c.HTTP, "0"); s == "unknown" {
				fmt.Println("sippy:     unreachable or unexpected response")
			} else {
				fmt.Printf("sippy:     ok (%s)\n", s)
			}
			cancel2()

			ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := enrich.SearchCI(ctx3, c.HTTP, "whyfail-doctor-probe", "1h"); err == nil {
				fmt.Println("search.ci: ok")
			} else {
				fmt.Printf("search.ci: FAILED: %v\n", err)
			}
			cancel3()
			return nil
		},
	}
}

// ---------- pr ----------

var prURLRe = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)/?$`)

// prFanOut is how many failing checks are looked up at once. Each health.Collect
// fans out internally, so the outer width stays small.
const prFanOut = 4

func newPRCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "pr <github-pr-url>",
		Short: "Health of every failing check on a PR's head commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m := prURLRe.FindStringSubmatch(args[0])
			if m == nil {
				return fmt.Errorf("not a GitHub PR URL: %s", args[0])
			}
			org, repo, num := m[1], m[2], m[3]
			if _, err := exec.LookPath("gh"); err != nil {
				return fmt.Errorf("the pr command needs the gh CLI for GitHub access")
			}
			ctx := context.Background()
			c := newClient(g)
			start := time.Now()

			meta, err := ghJSON("repos/" + org + "/" + repo + "/pulls/" + num)
			if err != nil {
				return err
			}
			sha := stringField(meta, "head", "sha")
			base := stringField(meta, "base", "ref")
			title, _ := meta["title"].(string)
			if sha == "" {
				return fmt.Errorf("could not read head SHA for %s", args[0])
			}

			cards, err := collectPRChecks(ctx, execGH, c, g.bucket, org, repo, base, sha, g.window)
			if err != nil {
				return err
			}

			var buf bytes.Buffer
			if g.jsonOut {
				writeJSON(&buf, map[string]any{"pr": args[0], "title": title, "sha": sha, "failing": cards})
			} else {
				st := g.style()
				fmt.Fprintf(&buf, "%s/%s%s  %s\n\n", org, repo, st.Cyan("#"+num), title)
				if len(cards) == 0 {
					fmt.Fprintln(&buf, "no failing checks on the head commit")
				}
				jobW := st.Width() - 44
				if jobW < 24 {
					jobW = 24
				}
				if jobW > 60 {
					jobW = 60
				}
				bad := 0
				for _, cd := range cards {
					buf.WriteString(prCardLine(st, cd, jobW))
					if cd.Health != nil && cd.Health.Verdict.IsBad() {
						bad++
					}
				}
				if bad > 0 && bad == len(cards) {
					fmt.Fprintf(&buf, "\n  %s\n", st.Yellow("all failing jobs have poor history — the PR is probably not the cause"))
				}
			}
			g.emit(&buf, stats.Record{Cmd: "pr", Target: org + "/" + repo + "#" + num, Repo: org + "/" + repo}, c, start)
			return nil
		},
	}
}

// prCard is one failing check on a PR head commit. Prow is false for a check
// that has no Prow job to look up at all — a GitHub Actions job, say. Health is
// nil when the job could not be looked up, usually a base-branch rename, which
// makes the synthesised job name wrong.
type prCard struct {
	Check  string          `json:"check"`
	Job    string          `json:"job,omitempty"`
	Prow   bool            `json:"prow"`
	Health *health.Summary `json:"health"`
}

// commitStatus is one entry from the legacy commit-status API, which Prow uses.
type commitStatus struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

// checkRun is one entry from the Checks API, which GitHub Actions uses.
type checkRun struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
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
func collectPRChecks(ctx context.Context, api ghAPI, c *gcs.Client, bucket, org, repo, branch, sha string, window time.Duration) ([]prCard, error) {
	base := "repos/" + org + "/" + repo + "/commits/" + sha

	statusRaw, err := api(base+"/statuses?per_page=100", true)
	if err != nil {
		return nil, err
	}
	statuses, err := decodePages[commitStatus](statusRaw)
	if err != nil {
		return nil, fmt.Errorf("decode commit statuses: %w", err)
	}
	runsRaw, err := api(base+"/check-runs?per_page=100&filter=latest", true)
	if err != nil {
		return nil, err
	}
	runs, err := decodeCheckRunPages(runsRaw)
	if err != nil {
		return nil, fmt.Errorf("decode check runs: %w", err)
	}

	var cards []prCard
	seen := map[string]bool{}
	// Statuses first: the API returns the newest entry per context first, and
	// Prow's names are the ones job-name synthesis understands.
	for _, s := range statuses {
		if seen[s.Context] {
			continue
		}
		seen[s.Context] = true
		if s.State != "failure" && s.State != "error" {
			continue
		}
		test := strings.TrimPrefix(s.Context, "ci/prow/")
		if test == s.Context || test == "" {
			// A failing check with no Prow job behind it: report it rather than
			// dropping it, which is what made an all-Actions PR look clean.
			cards = append(cards, prCard{Check: s.Context})
			continue
		}
		cards = append(cards, prCard{
			Check: s.Context,
			Job:   "pull-ci-" + org + "-" + repo + "-" + branch + "-" + test,
			Prow:  true,
		})
	}
	for _, r := range runs {
		if seen[r.Name] || !failingCheckRun(r) {
			continue
		}
		seen[r.Name] = true
		cards = append(cards, prCard{Check: r.Name})
	}

	// Each Collect already fans out internally, so keep the outer width small.
	// Only Prow checks have a job to look up.
	parallel.Do(len(cards), prFanOut, func(i int) {
		if !cards[i].Prow {
			return
		}
		cards[i].Health, _ = health.Collect(ctx, c, bucket, cards[i].Job, window)
	})
	return cards, nil
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

// prCardLine renders one check. A nil Health is reported as an unknown Prow job
// or as a check with no Prow job behind it, never as healthy.
func prCardLine(st term.Style, cd prCard, jobW int) string {
	name := cd.Job
	if name == "" {
		name = cd.Check
	}
	name = term.PadRight(term.TruncMiddle(name, jobW), jobW)
	if cd.Health == nil {
		if !cd.Prow {
			return fmt.Sprintf("  %s %s %s\n", name,
				st.Red(term.PadRight("✗ FAILED", 11)),
				st.Dim("no Prow job for this check"))
		}
		return fmt.Sprintf("  %s %s %s\n", name,
			st.Dim(term.PadRight("? UNKNOWN", 11)),
			st.Dim("no history in this bucket"))
	}
	icon, label := render.VerdictLabel(cd.Health.Verdict)
	return fmt.Sprintf("  %s %s %s %s pass (%d runs)\n", name,
		render.VerdictText(st, cd.Health.Verdict, icon),
		render.VerdictText(st, cd.Health.Verdict, term.PadRight(label, 9)),
		st.Dim(health.FormatRate(cd.Health.PassRate)), cd.Health.Runs)
}

// ghAPI is one `gh api` request. paginate follows the Link header and returns
// every page's JSON array concatenated, which is what the checks endpoints need.
type ghAPI func(endpoint string, paginate bool) ([]byte, error)

// execGH runs `gh api`. Its signature satisfies ghAPI, so callers that want to
// be testable take a ghAPI and tests pass a canned one.
func execGH(endpoint string, paginate bool) ([]byte, error) {
	args := []string{"api"}
	if paginate {
		args = append(args, "--paginate")
	}
	args = append(args, endpoint)
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w", endpoint, err)
	}
	return out, nil
}

func ghGet(endpoint string) ([]byte, error) { return execGH(endpoint, false) }

func ghJSON(endpoint string) (map[string]any, error) {
	out, err := ghGet(endpoint)
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

func serveMCP(g *globals) error {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	enc := json.NewEncoder(os.Stdout)

	tools := []map[string]any{
		{"name": "prow_run", "description": "Digest of a Prow job run", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []string{"url"}}},
		{"name": "prow_job", "description": "Health of a Prow job over retained history", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"job": map[string]any{"type": "string"}}, "required": []string{"job"}}},
	}

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
			resp["result"] = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
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
	ctx := context.Background()
	c := newClient(g)
	switch name {
	case "prow_run":
		raw, _ := args["url"].(string)
		ref, err := prow.ParseRef(raw)
		if err != nil {
			return "", err
		}
		d, err := run.Build(ctx, c, ref, run.Options{History: true})
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
	}
	return "", fmt.Errorf("unknown tool %q", name)
}
