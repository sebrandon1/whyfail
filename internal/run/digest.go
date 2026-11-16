// Package run builds a compact, structured digest of a single Prow job run.
package run

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sebrandon1/whyfail/internal/api"
	"github.com/sebrandon1/whyfail/internal/gcs"
	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/prow"
)

// Digest is the whyfail/v1 document emitted for a run.
type Digest struct {
	Schema     string     `json:"schema"`
	Job        *JobInfo   `json:"job,omitempty"`
	Run        *RunInfo   `json:"run"`
	Tests      *Tests     `json:"tests,omitempty"`
	Infra      *Infra     `json:"infra,omitempty"`
	Artifacts  *Artifacts `json:"artifacts,omitempty"`
	Signals    *Signals   `json:"signals,omitempty"`
	NextAction string     `json:"next_action,omitempty"`
}

// JobInfo carries job-level context, including retained history.
type JobInfo struct {
	Name   string          `json:"name"`
	Type   string          `json:"type,omitempty"`
	Health *health.Summary `json:"health,omitempty"`
}

// RunInfo describes the run itself.
type RunInfo struct {
	ID           string    `json:"id"`
	JobName      string    `json:"job_name"`
	Type         string    `json:"type,omitempty"`
	State        string    `json:"state,omitempty"`
	Result       string    `json:"result,omitempty"`
	URL          string    `json:"url"`
	Reason       string    `json:"reason,omitempty"`
	Cluster      string    `json:"cluster,omitempty"`
	Namespace    string    `json:"namespace,omitempty"`
	PR           *PRInfo   `json:"pr,omitempty"`
	RerunCommand string    `json:"rerun_command,omitempty"`
	Timing       Timing    `json:"timing"`
	Steps        StepsInfo `json:"steps"`
}

// PRInfo identifies the pull request a presubmit run belongs to.
type PRInfo struct {
	Org     string `json:"org"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Author  string `json:"author,omitempty"`
	Title   string `json:"title,omitempty"`
	SHA     string `json:"sha,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	BaseSHA string `json:"base_sha,omitempty"`
}

// Timing records when the run ran and how much of the timeout it used.
type Timing struct {
	Pending      string  `json:"pending,omitempty"`
	Started      string  `json:"started,omitempty"`
	Finished     string  `json:"finished,omitempty"`
	DurationS    float64 `json:"duration_s"`
	TimeoutS     float64 `json:"timeout_s,omitempty"`
	PctOfTimeout float64 `json:"pct_of_timeout,omitempty"`
}

// StepsInfo summarizes the ci-operator step graph.
type StepsInfo struct {
	Failed  []StepRef `json:"failed,omitempty"`
	Longest []StepRef `json:"longest,omitempty"`
	Total   int       `json:"total"`
}

// StepRef names a step and its duration.
type StepRef struct {
	Name      string  `json:"name"`
	DurationS float64 `json:"duration_s"`
	Error     string  `json:"error,omitempty"`
}

// Tests summarizes structured test results.
type Tests struct {
	Counts       TestCounts    `json:"counts"`
	FailedTotal  int           `json:"failed_total,omitempty"`
	MonitorTotal int           `json:"monitor_total,omitempty"`
	Failed       []TestFailure `json:"failed,omitempty"`
	Monitors     []TestFailure `json:"monitors,omitempty"`
	Causes       []CauseGroup  `json:"monitor_causes,omitempty"`
	Source       string        `json:"source,omitempty"`
}

// CauseGroup is one root cause shared by several failures.
type CauseGroup struct {
	Signature string `json:"signature"`
	Count     int    `json:"count"`
}

// TestCounts are aggregate test outcomes.
type TestCounts struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Flaked  int `json:"flaked"`
}

// TestFailure is one failed test or monitor.
type TestFailure struct {
	Name       string `json:"name"`
	Lifecycle  string `json:"lifecycle,omitempty"`
	Blocking   bool   `json:"blocking"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
	Signature  string `json:"signature,omitempty"`
}

// Infra captures signals about the cloud/cluster the job used.
type Infra struct {
	ClusterProfile string     `json:"cluster_profile,omitempty"`
	ClusterID      string     `json:"cluster_id,omitempty"`
	Region         string     `json:"region,omitempty"`
	Variant        string     `json:"variant,omitempty"`
	Lease          *LeaseInfo `json:"lease,omitempty"`
	MachinesTotal  int        `json:"machines_total,omitempty"`
	Execution      *ExecInfo  `json:"execution,omitempty"`
	InstallFailed  bool       `json:"install_failed,omitempty"`
}

// LeaseInfo is the cloud quota slice the job acquired.
type LeaseInfo struct {
	Name               string  `json:"name,omitempty"`
	Slice              string  `json:"slice,omitempty"`
	Region             string  `json:"region,omitempty"`
	AcquireS           float64 `json:"acquire_s,omitempty"`
	AvailableAtAcquire int     `json:"available_at_acquire"`
}

// ExecInfo is the ci-operator's own completion record.
type ExecInfo struct {
	DurationS  float64 `json:"duration_s"`
	ErrorCount int     `json:"error_count"`
	Success    bool    `json:"success"`
}

// Artifacts is an inventory of the run's artifacts.
type Artifacts struct {
	TotalBytes int64     `json:"total_bytes"`
	Count      int       `json:"count"`
	Notable    []Notable `json:"notable,omitempty"`
}

// Notable is a single artifact worth mentioning.
type Notable struct {
	Name  string `json:"name"`
	Short string `json:"-"`
	Bytes int64  `json:"bytes"`
	Why   string `json:"why"`
}

// Signals carries enrichment from external services.
type Signals struct {
	Sippy    string     `json:"sippy,omitempty"`
	SearchCI *SearchSig `json:"search_ci,omitempty"`
}

// SearchSig is the search.ci result for the top failure signature.
type SearchSig struct {
	Query      string `json:"query"`
	Matches48h int    `json:"matches_48h"`
	KnownBug   string `json:"known_bug,omitempty"`
}

// Options controls how much work Build does.
type Options struct {
	History       bool
	BuildLogLimit int64
}

// Build assembles a digest for ref.
func Build(ctx context.Context, c *gcs.Client, ref prow.Ref, opts Options) (*Digest, error) {
	if opts.BuildLogLimit == 0 {
		opts.BuildLogLimit = 4 << 20
	}
	objs, err := c.List(ctx, ref.Bucket, ref.ObjectPath+"/")
	if err != nil {
		return nil, err
	}
	base := ref.ObjectPath + "/"

	d := &Digest{Schema: api.SchemaV1}
	run := &RunInfo{ID: ref.RunID, JobName: ref.Job, URL: prow.ViewURL(ref)}
	d.Run = run

	var pj prowJob
	if data, err := c.Get(ctx, ref.Bucket, base+"prowjob.json"); err == nil {
		if json.Unmarshal(data, &pj) == nil {
			applyProwJob(run, pj)
		}
	}
	var fin finished
	if data, err := c.Get(ctx, ref.Bucket, base+"finished.json"); err == nil {
		if json.Unmarshal(data, &fin) == nil {
			if fin.Result != "" {
				run.Result = fin.Result
			}
			if run.State == "" && fin.Result != "" {
				run.State = strings.ToLower(fin.Result)
			}
		}
	}
	if run.State == "" {
		run.State = strings.ToLower(run.Result)
	}

	var steps []step
	if data, err := c.Get(ctx, ref.Bucket, base+"artifacts/ci-operator-step-graph.json"); err == nil {
		if json.Unmarshal(data, &steps) == nil {
			run.Steps = summarizeSteps(steps)
		}
	}
	mergeOperatorFailures(ctx, c, ref, base, &run.Steps)

	if data, err := c.GetLimit(ctx, ref.Bucket, base+"build-log.txt", opts.BuildLogLimit); err == nil {
		run.Reason = buildReason(data)
	}

	d.Tests = extractTests(ctx, c, ref, objs)
	d.Infra = extractInfra(ctx, c, ref, base, run.Steps)
	d.Artifacts = inventory(objs, base, run.Steps.Failed)

	d.Job = &JobInfo{Name: run.JobName, Type: run.Type}
	if opts.History {
		switch sum, err := health.Collect(ctx, c, ref.Bucket, run.JobName, 0); {
		case err == nil:
			d.Job.Health = sum
		case errors.Is(err, health.ErrJobNotFound):
			// The run exists but its job is not in the index: report "no
			// history" rather than dropping the job block from the card.
			d.Job.Health = &health.Summary{Job: run.JobName, Verdict: health.VerdictUnknown}
		}
	}

	d.NextAction = nextAction(d)
	return d, nil
}

// ---------- prowjob.json ----------

type prowJob struct {
	Spec struct {
		Type         string `json:"type"`
		Cluster      string `json:"cluster"`
		Namespace    string `json:"namespace"`
		Job          string `json:"job"`
		Context      string `json:"context"`
		RerunCommand string `json:"rerun_command"`
		Refs         *struct {
			Org     string `json:"org"`
			Repo    string `json:"repo"`
			BaseRef string `json:"base_ref"`
			BaseSHA string `json:"base_sha"`
			Pulls   []struct {
				Number  int    `json:"number"`
				Author  string `json:"author"`
				SHA     string `json:"sha"`
				Title   string `json:"title"`
				HeadRef string `json:"head_ref"`
			} `json:"pulls"`
		} `json:"refs"`
		DecorationConfig struct {
			Timeout string `json:"timeout"`
		} `json:"decoration_config"`
	} `json:"spec"`
	Status struct {
		State          string `json:"state"`
		Description    string `json:"description"`
		URL            string `json:"url"`
		StartTime      string `json:"startTime"`
		PendingTime    string `json:"pendingTime"`
		CompletionTime string `json:"completionTime"`
		PodName        string `json:"pod_name"`
		BuildID        string `json:"build_id"`
	} `json:"status"`
}

type finished struct {
	Result    string `json:"result"`
	Passed    bool   `json:"passed"`
	Timestamp int64  `json:"timestamp"`
	Revision  string `json:"revision"`
}

func applyProwJob(r *RunInfo, pj prowJob) {
	if pj.Spec.Job != "" {
		r.JobName = pj.Spec.Job
	}
	r.Type = pj.Spec.Type
	r.Cluster = pj.Spec.Cluster
	r.Namespace = pj.Spec.Namespace
	r.RerunCommand = pj.Spec.RerunCommand
	if pj.Status.State != "" {
		r.State = pj.Status.State
	}
	r.Timing.Pending = pj.Status.PendingTime
	r.Timing.Started = pj.Status.StartTime
	r.Timing.Finished = pj.Status.CompletionTime

	if refs := pj.Spec.Refs; refs != nil {
		pr := &PRInfo{
			Org:     refs.Org,
			Repo:    refs.Repo,
			BaseRef: refs.BaseRef,
			BaseSHA: refs.BaseSHA,
		}
		if len(refs.Pulls) > 0 {
			p := refs.Pulls[0]
			pr.Number = p.Number
			pr.Author = p.Author
			pr.Title = p.Title
			pr.SHA = p.SHA
		}
		if pr.Number != 0 || pr.Org != "" {
			r.PR = pr
		}
	}

	if d, err := time.ParseDuration(pj.Spec.DecorationConfig.Timeout); err == nil {
		r.Timing.TimeoutS = d.Seconds()
	}
	start, errS := time.Parse(time.RFC3339, pj.Status.StartTime)
	end, errE := time.Parse(time.RFC3339, pj.Status.CompletionTime)
	if errS == nil && errE == nil && end.After(start) {
		r.Timing.DurationS = end.Sub(start).Seconds()
		if r.Timing.TimeoutS > 0 {
			r.Timing.PctOfTimeout = r.Timing.DurationS / r.Timing.TimeoutS * 100
		}
	}
}

// ---------- step graph ----------

type step struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Dependencies []string `json:"dependencies"`
	Duration     float64  `json:"duration"`
	Failed       bool     `json:"failed"`
	StartedAt    string   `json:"started_at"`
	FinishedAt   string   `json:"finished_at"`
}

func summarizeSteps(steps []step) StepsInfo {
	out := StepsInfo{Total: len(steps)}
	for _, s := range steps {
		if s.Failed {
			out.Failed = append(out.Failed, StepRef{Name: s.Name, DurationS: s.Duration / 1e9})
		}
	}
	sorted := append([]step(nil), steps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Duration > sorted[j].Duration })
	for i := 0; i < len(sorted) && i < 5; i++ {
		if sorted[i].Duration <= 0 {
			break
		}
		out.Longest = append(out.Longest, StepRef{Name: sorted[i].Name, DurationS: sorted[i].Duration / 1e9})
	}
	return out
}

// mergeOperatorFailures adds failing sub-steps recorded in junit_operator.xml,
// which names the actual step even when the graph only fails the parent test.
func mergeOperatorFailures(ctx context.Context, c *gcs.Client, ref prow.Ref, base string, steps *StepsInfo) {
	data, err := c.Get(ctx, ref.Bucket, base+"artifacts/junit_operator.xml")
	if err != nil {
		return
	}
	report, err := parseJUnit(strings.NewReader(string(data)))
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, s := range steps.Failed {
		seen[s.Name] = true
	}
	for _, f := range report.Failures {
		name := strings.TrimPrefix(f.Name, "Run multi-stage step ")
		if name == f.Name || seen[name] {
			continue
		}
		seen[name] = true
		steps.Failed = append(steps.Failed, StepRef{
			Name:      name,
			DurationS: float64(f.DurationMS) / 1000,
			Error:     stepError(f.Error),
		})
	}
}

func stepError(s string) string {
	var weak, fallback string
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "make[") || strings.HasPrefix(l, "make:") {
			continue
		}
		core := strings.TrimLeft(l, "._-~ \t")
		if core == "" {
			continue // progress dots
		}
		if clockRe.MatchString(core) || strings.Contains(core, "m=+") {
			continue // bare timestamps and monotonic clocks
		}
		if len(core) > 220 {
			core = core[:220] + "..."
		}
		low := strings.ToLower(core)
		if strings.Contains(low, "error") || strings.Contains(low, "failed") ||
			strings.Contains(low, "unable") || strings.Contains(low, "no such host") ||
			strings.Contains(low, "exit status") || strings.Contains(low, "no route") {
			return core
		}
		if weak == "" && strings.Contains(low, "timeout") {
			weak = core
		}
		if fallback == "" {
			fallback = core
		}
	}
	if weak != "" {
		return weak
	}
	return fallback
}

var reasonRe = regexp.MustCompile(`Reporting job state '([^']+)' with reason '([^']+)'`)

func buildReason(log []byte) string {
	ms := reasonRe.FindAllSubmatch(log, -1)
	if len(ms) == 0 {
		return ""
	}
	return string(ms[len(ms)-1][2])
}

// ---------- tests ----------

type testResult struct {
	Name      string  `json:"name"`
	Lifecycle string  `json:"lifecycle"`
	Result    string  `json:"result"`
	Duration  float64 `json:"duration"`
	Error     string  `json:"error"`
}

func extractTests(ctx context.Context, c *gcs.Client, ref prow.Ref, objs []gcs.Object) *Tests {
	var e2e, mon []gcs.Object
	for _, o := range objs {
		b := path.Base(o.Name)
		switch {
		case strings.HasPrefix(b, "extension_test_result_e2e__") && strings.HasSuffix(b, ".json"):
			e2e = append(e2e, o)
		case strings.HasPrefix(b, "extension_test_result_monitors__") && strings.HasSuffix(b, ".json"):
			mon = append(mon, o)
		}
	}

	t := &Tests{}
	usedStruct := false
	if o, ok := largest(e2e); ok {
		if data, err := c.Get(ctx, ref.Bucket, o.Name); err == nil {
			var rs []testResult
			if json.Unmarshal(data, &rs) == nil {
				usedStruct = true
				t.Source = "extension"
				for _, r := range rs {
					switch r.Result {
					case "passed":
						t.Counts.Passed++
					case "failed":
						t.Counts.Failed++
						t.Failed = append(t.Failed, failureOf(r))
					case "skipped":
						t.Counts.Skipped++
					default:
						if strings.Contains(strings.ToLower(r.Result), "flak") {
							t.Counts.Flaked++
						}
					}
				}
			}
		}
	}
	if o, ok := largest(mon); ok {
		if data, err := c.Get(ctx, ref.Bucket, o.Name); err == nil {
			var rs []testResult
			if json.Unmarshal(data, &rs) == nil {
				usedStruct = true
				for _, r := range rs {
					if r.Result == "failed" {
						f := failureOf(r)
						f.Lifecycle = "monitor"
						f.Blocking = true
						f.Signature = monitorSignature(r.Name, r.Error)
						t.Monitors = append(t.Monitors, f)
					}
				}
			}
		}
	}

	if !usedStruct {
		for _, o := range junitCandidates(objs) {
			data, err := c.Get(ctx, ref.Bucket, o.Name)
			if err != nil {
				continue
			}
			report, err := parseJUnit(strings.NewReader(string(data)))
			if err != nil {
				continue
			}
			t.Source = "junit"
			t.Failed = append(t.Failed, report.Failures...)
			t.Counts.Failed += report.Failed
			t.Counts.Skipped += report.Skipped
			t.Counts.Passed += report.Passed()
		}
	}

	if len(t.Monitors) == 0 {
		for _, o := range monitorXML(objs) {
			data, err := c.Get(ctx, ref.Bucket, o.Name)
			if err != nil {
				continue
			}
			report, err := parseJUnit(strings.NewReader(string(data)))
			if err != nil {
				continue
			}
			fails := report.Failures
			for i := range fails {
				fails[i].Lifecycle = "monitor"
				fails[i].Blocking = true
			}
			t.Monitors = append(t.Monitors, fails...)
		}
	}

	if t.Counts.Passed == 0 && t.Counts.Failed == 0 && t.Counts.Skipped == 0 &&
		len(t.Failed) == 0 && len(t.Monitors) == 0 {
		return nil
	}

	// Keep the digest small: report counts and one representative per cause.
	t.FailedTotal = len(t.Failed)
	if len(t.Failed) > 20 {
		t.Failed = t.Failed[:20]
	}
	t.MonitorTotal = len(t.Monitors)
	t.Causes = GroupCauses(t.Monitors)
	t.Monitors = representatives(t.Monitors, 10)
	return t
}

func GroupCauses(fs []TestFailure) []CauseGroup {
	idx := map[string]int{}
	var out []CauseGroup
	for _, f := range fs {
		sig := f.Signature
		if sig == "" {
			sig = f.Name
		}
		if i, ok := idx[sig]; ok {
			out[i].Count++
			continue
		}
		idx[sig] = len(out)
		out = append(out, CauseGroup{Signature: sig, Count: 1})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Signature < out[j].Signature
	})
	return out
}

func representatives(fs []TestFailure, n int) []TestFailure {
	seen := map[string]bool{}
	var out []TestFailure
	for _, f := range fs {
		sig := f.Signature
		if sig == "" {
			sig = f.Name
		}
		if seen[sig] {
			continue
		}
		seen[sig] = true
		if len(f.Error) > 250 {
			f.Error = f.Error[:250] + "..."
		}
		out = append(out, f)
		if len(out) >= n {
			break
		}
	}
	return out
}

func failureOf(r testResult) TestFailure {
	errText := normalizeExcerpt(r.Error)
	return TestFailure{
		Name:       r.Name,
		Lifecycle:  r.Lifecycle,
		Blocking:   r.Lifecycle == "blocking",
		DurationMS: int64(r.Duration),
		Error:      errText,
		Signature:  signatureOf(errText),
	}
}

// junitCandidates returns the JUnit files worth parsing. Some test steps emit a
// bare artifacts/<step>/artifacts/junit.xml instead of the junit_e2e__*/.xml
// names; it is only used when no named candidate exists, so a run that emits
// both is not counted twice.
func junitCandidates(objs []gcs.Object) []gcs.Object {
	var out, bare []gcs.Object
	for _, o := range objs {
		b := path.Base(o.Name)
		switch {
		case strings.HasPrefix(b, "junit_e2e__") && strings.HasSuffix(b, ".xml"):
			out = append(out, o)
		case b == "junit_install.xml":
			out = append(out, o)
		case b == "junit.xml":
			bare = append(bare, o)
		}
	}
	if len(out) == 0 {
		return bare
	}
	return out
}

func monitorXML(objs []gcs.Object) []gcs.Object {
	var out []gcs.Object
	for _, o := range objs {
		if strings.HasPrefix(path.Base(o.Name), "e2e-monitor-tests") && strings.HasSuffix(o.Name, ".xml") {
			out = append(out, o)
		}
	}
	return out
}

// junitReport is what one JUnit file says: its failures plus the totals needed
// to render pass/fail/skip counts.
type junitReport struct {
	Failures []TestFailure
	Total    int // <testcase> elements
	Failed   int // <failure> and <error> elements
	Skipped  int // <skipped> elements
}

// Passed is the number of testcases that neither failed nor were skipped.
func (r junitReport) Passed() int {
	if n := r.Total - r.Failed - r.Skipped; n > 0 {
		return n
	}
	return 0
}

func parseJUnit(r io.Reader) (junitReport, error) {
	dec := xml.NewDecoder(r)
	var out junitReport
	var cur *TestFailure
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "testcase":
				out.Total++
				cur = &TestFailure{}
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "name":
						cur.Name = a.Value
					case "time":
						if v, err := strconv.ParseFloat(a.Value, 64); err == nil {
							cur.DurationMS = int64(v * 1000)
						}
					}
				}
			case "skipped":
				out.Skipped++
				// The element is usually empty; consume it so its end tag is
				// not mistaken for anything else.
				if err := dec.Skip(); err != nil {
					return out, err
				}
			case "failure", "error":
				out.Failed++
				if cur == nil {
					continue
				}
				var msg string
				for _, a := range t.Attr {
					if a.Name.Local == "message" {
						msg = a.Value
					}
				}
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					return out, err
				}
				if msg != "" {
					text = msg + "\n" + text
				}
				cur.Error = normalizeExcerpt(text)
				cur.Signature = signatureOf(cur.Error)
				out.Failures = append(out.Failures, *cur)
			}
		case xml.EndElement:
			if t.Name.Local == "testcase" {
				cur = nil
			}
		}
	}
	return out, nil
}

var (
	ipv4Re     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	podRe      = regexp.MustCompile(`-[a-z0-9]{5}-[a-z0-9]{5}\b`)
	nsRe       = regexp.MustCompile(`ci-op-[a-z0-9]{8}`)
	tsRe       = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T[\d:.]+Z?\b`)
	spaceRe    = regexp.MustCompile(`[ \t]+`)
	lookupRe   = regexp.MustCompile(`lookup ([A-Za-z0-9._-]+)`)
	postHostRe = regexp.MustCompile(`Post "https://([^/"]+)`)
	monitorRe  = regexp.MustCompile(`\[Monitor:([^\]]+)\]`)
	clockRe    = regexp.MustCompile(`^\d{1,2}:\d{2}:\d{2}`)
)

// monitorSignature collapses monitor failures to their actionable cause.
func monitorSignature(name, errText string) string {
	if m := lookupRe.FindStringSubmatch(errText); m != nil {
		return "dns lookup failed: " + m[1]
	}
	if strings.Contains(errText, "no such host") {
		if m := postHostRe.FindStringSubmatch(errText); m != nil {
			return "dns lookup failed: " + m[1]
		}
	}
	if m := monitorRe.FindStringSubmatch(name); m != nil {
		return "monitor: " + m[1]
	}
	return signatureOf(normalizeExcerpt(errText))
}

func normalizeExcerpt(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > 6 {
		lines = lines[:6]
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(spaceRe.ReplaceAllString(lines[i], " "))
	}
	s = strings.Join(lines, "\n")
	if len(s) > 600 {
		s = s[:600] + "..."
	}
	return s
}

func signatureOf(s string) string {
	s = ipv4Re.ReplaceAllString(s, "<ip>")
	s = podRe.ReplaceAllString(s, "-<pod>")
	s = nsRe.ReplaceAllString(s, "ci-op-<ns>")
	s = tsRe.ReplaceAllString(s, "<ts>")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func largest(objs []gcs.Object) (gcs.Object, bool) {
	if len(objs) == 0 {
		return gcs.Object{}, false
	}
	best := objs[0]
	for _, o := range objs[1:] {
		if o.Size > best.Size {
			best = o
		}
	}
	return best, true
}

// ---------- infra ----------

type metricsFile struct {
	Insights []struct {
		Name    string          `json:"name"`
		Context json.RawMessage `json:"additional_context"`
	} `json:"test_platform_insights"`
	Leases []struct {
		Name        string  `json:"name"`
		Slice       string  `json:"slice"`
		Region      string  `json:"region"`
		Acquisition float64 `json:"acquisition_duration_seconds"`
		Remaining   int     `json:"leases_remaining_at_acquisition"`
		Released    bool    `json:"released"`
	} `json:"leases"`
	Machines []struct {
		Workload string `json:"workload"`
		Total    int    `json:"total_machines"`
	} `json:"machines"`
}

type configCtx struct {
	Variant     string `json:"variant"`
	ClusterInfo struct {
		ClusterID string `json:"cluster_id"`
		Profiles  []struct {
			Target      string `json:"target"`
			ProfileName string `json:"profile_name"`
		} `json:"cluster_profiles"`
	} `json:"cluster_info"`
}

type execCtx struct {
	DurationS  float64 `json:"duration_seconds"`
	ErrorCount int     `json:"error_count"`
	Success    bool    `json:"success"`
}

func extractInfra(ctx context.Context, c *gcs.Client, ref prow.Ref, base string, steps StepsInfo) *Infra {
	data, err := c.Get(ctx, ref.Bucket, base+"artifacts/ci-operator-metrics.json")
	if err != nil {
		return nil
	}
	var m metricsFile
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	inf := &Infra{}
	for _, in := range m.Insights {
		switch in.Name {
		case "configuration":
			var cc configCtx
			if json.Unmarshal(in.Context, &cc) == nil {
				inf.Variant = cc.Variant
				inf.ClusterID = cc.ClusterInfo.ClusterID
				if len(cc.ClusterInfo.Profiles) > 0 {
					inf.ClusterProfile = cc.ClusterInfo.Profiles[0].ProfileName
				}
			}
		case "execution_completed":
			var ec execCtx
			if json.Unmarshal(in.Context, &ec) == nil {
				inf.Execution = &ExecInfo{DurationS: ec.DurationS, ErrorCount: ec.ErrorCount, Success: ec.Success}
			}
		}
	}
	if len(m.Leases) > 0 {
		l := m.Leases[0]
		inf.Lease = &LeaseInfo{
			Name: l.Name, Slice: l.Slice, Region: l.Region,
			AcquireS: l.Acquisition, AvailableAtAcquire: l.Remaining,
		}
		inf.Region = l.Region
	}
	for _, mc := range m.Machines {
		if mc.Total > inf.MachinesTotal {
			inf.MachinesTotal = mc.Total
		}
	}
	for _, s := range steps.Failed {
		if strings.Contains(s.Name, "install") {
			inf.InstallFailed = true
		}
	}
	if inf.ClusterProfile == "" && inf.Lease == nil && inf.Execution == nil && inf.MachinesTotal == 0 {
		return nil
	}
	return inf
}

// ---------- artifacts ----------

func inventory(objs []gcs.Object, base string, failedSteps []StepRef) *Artifacts {
	a := &Artifacts{Count: len(objs)}
	for _, o := range objs {
		a.TotalBytes += o.Size
	}
	failed := map[string]bool{}
	for _, s := range failedSteps {
		failed[s.Name] = true
	}
	type rule struct {
		match func(string) bool
		why   string
	}
	rules := []rule{
		{func(n string) bool { return path.Base(n) == "prowjob.json" }, "run identity and PR linkage"},
		{func(n string) bool { return n == base+"build-log.txt" }, "orchestration log"},
		{func(n string) bool { return path.Base(n) == "ci-operator-step-graph.json" }, "step timings and failures"},
		{func(n string) bool { return path.Base(n) == "ci-operator-metrics.json" }, "leases, machines, cluster profile"},
		{func(n string) bool { return path.Base(n) == "junit_operator.xml" }, "per-step results"},
		{func(n string) bool {
			if !strings.HasSuffix(n, "/build-log.txt") {
				return false
			}
			dir := path.Dir(n)
			for step := range failed {
				if strings.HasSuffix(dir, "/"+step) {
					return true
				}
			}
			return false
		}, "failing step log"},
		{func(n string) bool {
			return strings.Contains(path.Base(n), "extension_test_result_e2e__") && strings.HasSuffix(n, ".json")
		}, "structured test results"},
		{func(n string) bool {
			return strings.Contains(path.Base(n), "extension_test_result_monitors__") && strings.HasSuffix(n, ".json")
		}, "monitor test results"},
		{func(n string) bool { return strings.HasPrefix(path.Base(n), "e2e-monitor-tests") }, "monitor test results (XML)"},
		{func(n string) bool { return strings.HasPrefix(path.Base(n), "junit_e2e__") }, "JUnit e2e results"},
		{func(n string) bool { return path.Base(n) == "junit_install.xml" }, "install JUnit results"},
		{func(n string) bool { return strings.Contains(n, "installer-status.txt") }, "installer status"},
		{func(n string) bool { return strings.Contains(n, "must-gather") }, "cluster state capture"},
		{func(n string) bool { return strings.Contains(path.Base(n), "sosreport") }, "node diagnostics"},
	}
	seen := map[string]bool{}
	for _, r := range rules {
		for _, o := range objs {
			if seen[o.Name] || !r.match(o.Name) {
				continue
			}
			// Skip duplicate copies taken by gather steps.
			if strings.Contains(o.Name, "/gather-") && !strings.Contains(o.Name, "gather-must-gather") {
				continue
			}
			seen[o.Name] = true
			a.Notable = append(a.Notable, Notable{
				Name:  o.Name,
				Short: strings.TrimPrefix(o.Name, base),
				Bytes: o.Size,
				Why:   r.why,
			})
			if len(a.Notable) >= 12 {
				return a
			}
		}
	}
	return a
}

// ---------- next action ----------

func nextAction(d *Digest) string {
	if d.Run == nil {
		return ""
	}
	state := strings.ToLower(d.Run.State)
	if state == "success" {
		return "nothing to do; this run passed"
	}
	// A named, concrete failure outranks the job's overall verdict: a flaky job
	// can still fail a blocking test deterministically, and telling the reader
	// to retest would hide the thing they need to look at.
	if advice := blockingAdvice(d.Tests); advice != "" {
		return withFlakyCaveat(advice, d.Job)
	}
	if d.Job != nil && d.Job.Health != nil {
		switch d.Job.Health.Verdict {
		case health.VerdictBroken:
			return "this job is broken for everyone in the retained window; retesting will not help"
		case health.VerdictFlaky:
			return "job is known flaky; a retest is reasonable"
		}
	}
	if step, ok := rootCauseStep(d.Run.Steps.Failed); ok {
		return fmt.Sprintf("step %q failed; run `whyfail log <run> %s`", step.Name, step.Name)
	}
	if d.Tests != nil && (len(d.Tests.Failed) > 0 || len(d.Tests.Monitors) > 0) {
		return "tests failed; see the evidence above"
	}
	return "no clear cause found; inspect the build log"
}

func (t *Tests) blockingFailures() []TestFailure {
	if t == nil {
		return nil
	}
	var out []TestFailure
	for _, f := range t.Failed {
		if f.Blocking {
			out = append(out, f)
		}
	}
	return out
}

// blockingAdvice names the blocking failure to look at, preferring the first
// one and reporting how many others there are. It returns "" when no blocking
// test failed.
func blockingAdvice(t *Tests) string {
	blocking := t.blockingFailures()
	if len(blocking) == 0 {
		return ""
	}
	name := blocking[0].Name
	switch {
	case name == "":
		return "a blocking test failed; inspect it before retesting"
	case len(blocking) > 1:
		return fmt.Sprintf("%d blocking tests failed, starting with %q; inspect them before retesting",
			len(blocking), name)
	default:
		return fmt.Sprintf("blocking test %q failed; inspect it before retesting", name)
	}
}

// withFlakyCaveat keeps the job's flakiness visible without letting it displace
// the concrete failure the reader has to act on.
func withFlakyCaveat(advice string, job *JobInfo) string {
	if job == nil || job.Health == nil || job.Health.Verdict != health.VerdictFlaky {
		return advice
	}
	return "known flaky job, but " + advice
}

// rootCauseStep picks the failed step most likely to be the cause. Steps are
// recorded in completion order and operator sub-steps are appended last, so the
// search runs backwards; cleanup (gather-*) and input-resolution ([input:*])
// steps are skipped because they fail as a consequence of the real failure.
func rootCauseStep(failed []StepRef) (StepRef, bool) {
	for i := len(failed) - 1; i >= 0; i-- {
		if isConsequentialStep(failed[i].Name) {
			continue
		}
		return failed[i], true
	}
	if len(failed) > 0 {
		return failed[0], true
	}
	return StepRef{}, false
}

func isConsequentialStep(name string) bool {
	return strings.HasPrefix(name, "gather-") || strings.HasPrefix(name, "[input:")
}
