package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"github.com/sebrandon1/whyfail/internal/api"
	"github.com/sebrandon1/whyfail/internal/claudecfg"
	"github.com/sebrandon1/whyfail/internal/enrich"
	"github.com/sebrandon1/whyfail/internal/gcstest"
	"github.com/sebrandon1/whyfail/internal/health"
	"github.com/sebrandon1/whyfail/internal/render"
	"github.com/sebrandon1/whyfail/internal/run"
	"github.com/sebrandon1/whyfail/internal/stats"
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

// fakeGH answers gh api requests from a fixed endpoint map.
func fakeGH(t *testing.T, pages map[string]string) ghAPI {
	t.Helper()
	return func(_ context.Context, endpoint string, paginate bool) ([]byte, error) {
		if !paginate {
			t.Errorf("%s: want a paginated request", endpoint)
		}
		body, ok := pages[endpoint]
		if !ok {
			t.Fatalf("unexpected endpoint %q", endpoint)
		}
		return []byte(body), nil
	}
}

// fakeGHScript puts an executable `gh` that runs body under /bin/sh on PATH for
// the rest of the test, so execGH's real process handling is what gets tested.
func fakeGHScript(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// --timeout must bound the one command that leaves the process: a hung `gh`
// used to hold pr open with no recourse for an agent waiting on it.
func TestExecGHHonoursContext(t *testing.T) {
	// exec, not a plain child: a shell that leaves `sleep` behind keeps the
	// stdout pipe open, so Output would wait for the grandchild even after the
	// kill. A real gh is one process, and the shim should be too.
	fakeGHScript(t, "exec sleep 5")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := execGH(ctx, "repos/org/repo/pulls/1", false)
	if err == nil {
		t.Fatal("want an error from a gh that never returns")
	}
	// The kill surfaces as "signal: killed"; the friendly message must survive.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v: the context did not kill gh", elapsed)
	}
}

// A gh that answers is handed back untouched, and ghJSON decodes it.
func TestExecGHReturnsOutput(t *testing.T) {
	fakeGHScript(t, `echo '{"title":"hello"}'`)

	out, err := execGH(context.Background(), "repos/org/repo/pulls/1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("out = %q, want gh's stdout", out)
	}
	m, err := ghJSON(context.Background(), "repos/org/repo/pulls/1")
	if err != nil {
		t.Fatal(err)
	}
	if got := m["title"]; got != "hello" {
		t.Errorf("title = %v, want hello", got)
	}
}

// Output that is not JSON is an error, not a half-filled map.
func TestGHJSONRejectsNonJSON(t *testing.T) {
	fakeGHScript(t, "echo 'not json'")

	if _, err := ghJSON(context.Background(), "repos/org/repo/pulls/1"); err == nil {
		t.Fatal("want a decode error")
	}
}

// --grep is a filter over what was already fetched: it selects matching lines,
// numbers them, and expands them by context without printing a line twice.
func TestLogGrep(t *testing.T) {
	lines := []string{
		"starting",       // 1
		"tls: bad",       // 2
		"unrelated",      // 3
		"certificate x",  // 4
		"done",           // 5
		"tls: bad again", // 6
		"tail",           // 7
	}

	// One match, one line of context each side, numbered from the original.
	out, total, kept := grepLog(lines, regexp.MustCompile("certificate"), 1, 0)
	if total != 1 || kept != 1 {
		t.Fatalf("total, kept = %d, %d, want 1, 1", total, kept)
	}
	want := []string{"    3  unrelated", "    4  certificate x", "    5  done"}
	if strings.Join(out, "\n") != strings.Join(want, "\n") {
		t.Errorf("out =\n%s\nwant\n%s", strings.Join(out, "\n"), strings.Join(want, "\n"))
	}

	// Two windows that do not touch print both, in file order.
	out, _, _ = grepLog(lines, regexp.MustCompile("tls"), 0, 0)
	if len(out) != 2 || !strings.Contains(out[0], "tls: bad") || !strings.Contains(out[1], "again") {
		t.Errorf("out = %v, want the two matches", out)
	}

	// Adjacent windows merge: with context 2 the two matches cover 1-7 and no
	// line appears twice.
	out, _, _ = grepLog(lines, regexp.MustCompile("tls"), 2, 0)
	if len(out) != 7 {
		t.Fatalf("merged output has %d lines, want 7:\n%s", len(out), strings.Join(out, "\n"))
	}
	seen := map[string]bool{}
	for _, l := range out {
		if seen[l] {
			t.Errorf("line printed twice: %q", l)
		}
		seen[l] = true
	}

	// The cap limits how many matches are expanded, and reports the rest.
	out, total, kept = grepLog(lines, regexp.MustCompile("tls"), 0, 1)
	if total != 2 || kept != 1 {
		t.Errorf("total, kept = %d, %d, want 2, 1", total, kept)
	}
	if len(out) != 1 || !strings.Contains(out[0], "tls: bad") {
		t.Errorf("out = %v, want just the first match", out)
	}

	// No match is an empty selection, not an error.
	if out, total, _ := grepLog(lines, regexp.MustCompile("nope"), 3, 0); len(out) != 0 || total != 0 {
		t.Errorf("no match: out = %v, total = %d", out, total)
	}

	// A pattern is matched against the raw line, so numbering cannot affect it.
	if _, total, _ := grepLog(lines, regexp.MustCompile("^ *1 "), 0, 0); total != 0 {
		t.Errorf("matched a line number: total = %d", total)
	}
}

func TestCollectPRChecks(t *testing.T) {
	const (
		sha      = "abc123"
		base     = "repos/org/repo/commits/" + sha
		statuses = base + "/statuses?per_page=100"
		runs     = base + "/check-runs?per_page=100&filter=latest"
	)
	// Two pages of statuses, as `gh api --paginate` emits them: concatenated
	// arrays with no separator.
	statusPages := `[{"context":"ci/prow/e2e-aws","state":"failure"}]` +
		`[{"context":"ci/prow/unit","state":"success"},{"context":"ci/prow/e2e-aws","state":"success"}]`
	runPages := `{"total_count":3,"check_runs":[{"name":"build","status":"completed","conclusion":"failure"},` +
		`{"name":"lint","status":"completed","conclusion":"success"},` +
		`{"name":"e2e","status":"in_progress","conclusion":null}]}`

	cards, _, err := collectPRChecks(context.Background(), fakeGH(t, map[string]string{
		statuses: statusPages,
		runs:     runPages,
	}), gcstest.Client(t, &gcstest.Dataset{Objects: map[string][]byte{}, Listing: []gcstest.Item{
		{Name: "pr-logs/directory/pull-ci-org-repo-main-e2e-aws/latest-build.txt"},
	}}), gcstest.Bucket, "org", "repo", "main", sha, 0)
	if err != nil {
		t.Fatal(err)
	}

	// e2e-aws failed on the first page and passed on the second; the first
	// failing entry wins. build is a failing check run, lint passed, and e2e is
	// still running so it is not reported.
	want := []struct {
		check string
		prow  bool
	}{{"ci/prow/e2e-aws", true}, {"build", false}}
	if len(cards) != len(want) {
		t.Fatalf("cards = %+v, want %d", cards, len(want))
	}
	for i, w := range want {
		if cards[i].Check != w.check || cards[i].Prow != w.prow {
			t.Errorf("card %d = %+v, want check %q prow %v", i, cards[i], w.check, w.prow)
		}
	}
	if got := cards[0].Job; got != "pull-ci-org-repo-main-e2e-aws" {
		t.Errorf("synthesised job = %q", got)
	}
	if cards[1].Job != "" {
		t.Errorf("non-Prow check got a job name: %q", cards[1].Job)
	}
}

// A failed health lookup is an error on the card, not a claim about the job. An
// unserved bucket makes the listing itself fail, which is the case that used to
// render as "no history in this bucket".
func TestCollectPRChecksHealthLookupFailure(t *testing.T) {
	const (
		sha  = "abc123"
		base = "repos/org/repo/commits/" + sha
	)
	cards, warning, err := collectPRChecks(context.Background(), fakeGH(t, map[string]string{
		base + "/statuses?per_page=100":                 `[{"context":"ci/prow/e2e-aws","state":"failure"}]`,
		base + "/check-runs?per_page=100&filter=latest": `{"check_runs":[]}`,
	}), gcstest.Client(t, &gcstest.Dataset{Objects: map[string][]byte{}}), "no-such-bucket", "org", "repo", "main", sha, 0)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Errorf("warning = %q, want none for a failed lookup", warning)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one", cards)
	}
	if cards[0].Health != nil {
		t.Errorf("health = %+v, want nil on a failed lookup", cards[0].Health)
	}
	if cards[0].HealthErr == "" {
		t.Error("health_error is empty; a failed lookup must not read as no history")
	}
}

// An absent job is not an error: ErrJobNotFound keeps the "no history" wording,
// so the field must stay empty for it.
func TestCollectPRChecksJobNotFoundIsNotAHealthError(t *testing.T) {
	const (
		sha  = "abc123"
		base = "repos/org/repo/commits/" + sha
	)
	cards, _, err := collectPRChecks(context.Background(), fakeGH(t, map[string]string{
		base + "/statuses?per_page=100":                 `[{"context":"ci/prow/e2e-aws","state":"failure"}]`,
		base + "/check-runs?per_page=100&filter=latest": `{"check_runs":[]}`,
	}), gcstest.Client(t, &gcstest.Dataset{Objects: map[string][]byte{}}), gcstest.Bucket, "org", "repo", "main", sha, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one", cards)
	}
	if cards[0].Health != nil || cards[0].HealthErr != "" {
		t.Errorf("card = %+v, want no history and no error", cards[0])
	}
}

// Two workflows can emit a job with the same name. Each is a separate check, so
// both must surface; a re-run of one of them still reports once.
func TestCollectPRChecksSameNameDifferentApps(t *testing.T) {
	const (
		sha  = "abc123"
		base = "repos/org/repo/commits/" + sha
	)
	runPages := `{"check_runs":[` +
		`{"name":"test","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}},` +
		`{"name":"test","status":"completed","conclusion":"failure","app":{"slug":"circleci"}},` +
		`{"name":"test","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}},` +
		`{"name":"test","status":"completed","conclusion":"failure"}]}`

	cards, _, err := collectPRChecks(context.Background(), fakeGH(t, map[string]string{
		base + "/statuses?per_page=100":                 `[]`,
		base + "/check-runs?per_page=100&filter=latest": runPages,
	}), gcstest.Client(t, &gcstest.Dataset{Objects: map[string][]byte{}}), gcstest.Bucket, "org", "repo", "main", sha, 0)
	if err != nil {
		t.Fatal(err)
	}

	// The third run repeats the first key and is a re-run, not a new check. The
	// fourth has no app at all and is its own bucket rather than a wildcard.
	want := []string{"github-actions", "circleci", ""}
	if len(cards) != len(want) {
		t.Fatalf("cards = %+v, want %d", cards, len(want))
	}
	for i, app := range want {
		if cards[i].Check != "test" || cards[i].App != app {
			t.Errorf("card %d = %+v, want check %q app %q", i, cards[i], "test", app)
		}
		if cards[i].Source != render.SourceCheckRun || cards[i].Prow {
			t.Errorf("card %d = %+v, want a non-Prow check_run", i, cards[i])
		}
	}
}

// Prow reports through statuses, and its job-name synthesis keys off them, so a
// status wins against a check run that reuses the name.
func TestCollectPRChecksStatusBeatsCheckRun(t *testing.T) {
	const (
		sha  = "abc123"
		base = "repos/org/repo/commits/" + sha
	)
	cards, _, err := collectPRChecks(context.Background(), fakeGH(t, map[string]string{
		base + "/statuses?per_page=100": `[{"context":"build","state":"failure"}]`,
		base + "/check-runs?per_page=100&filter=latest": `{"check_runs":[` +
			`{"name":"build","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}}]}`,
	}), gcstest.Client(t, &gcstest.Dataset{Objects: map[string][]byte{}}), gcstest.Bucket, "org", "repo", "main", sha, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want the status alone", cards)
	}
	if cards[0].Source != render.SourceStatus || cards[0].Check != "build" {
		t.Errorf("card = %+v, want the status to win", cards[0])
	}
}

// logDataset builds a small run tree shaped like Prow's real artifact layout.
func logDataset(t *testing.T) (*gcstest.Dataset, string) {
	t.Helper()
	const run = "pr-logs/pull/org_repo/1/job/42"
	files := map[string]string{
		run + "/build-log.txt":                                  "job log",
		run + "/artifacts/release/build-log.txt":                "release log",
		run + "/artifacts/aws-e2e/provision/build-log.txt":      "provision log",
		run + "/artifacts/aws-e2e/gather/build-log.txt":         "gather log",
		run + "/artifacts/aws-e2e/artifacts/deep/build-log.txt": "deep log",
		run + "/artifacts/build-logs/src-amd64.log":             "build log, not a step",
	}
	d := &gcstest.Dataset{Objects: map[string][]byte{}}
	for name, body := range files {
		d.Objects[name] = []byte(body)
		d.Listing = append(d.Listing, gcstest.Item{Name: name})
	}
	// GCS returns a listing in lexical order; the fallback scan depends on it.
	sort.Slice(d.Listing, func(i, j int) bool { return d.Listing[i].Name < d.Listing[j].Name })
	return d, run
}

// countRequests counts the requests recorded exactly as want, so the narrow
// artifacts listing is not mistaken for a full-run listing.
func countRequests(reqs []string, want string) int {
	n := 0
	for _, r := range reqs {
		if r == want {
			n++
		}
	}
	return n
}

func TestResolveStepLog(t *testing.T) {
	cases := []struct {
		name     string
		step     string
		want     string
		wantErr  string
		wantList int // full-run listings; the whole point is that this stays 0
	}{
		{name: "job pseudo-step", step: "job", want: "job log"},
		{name: "direct child of artifacts", step: "release", want: "release log"},
		{name: "under the test directory", step: "provision", want: "provision log"},
		{name: "nested deeper, last-resort scan", step: "deep", want: "deep log", wantList: 1},
		{name: "unknown step", step: "nope", wantErr: `no build-log.txt found for step "nope"`, wantList: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, run := logDataset(t)
			client := gcstest.Client(t, d)

			target, data, err := resolveStepLog(context.Background(), client, gcstest.Bucket, run, c.step)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != c.want {
					t.Errorf("data = %q, want %q", data, c.want)
				}
				if !strings.HasPrefix(target, run+"/") {
					t.Errorf("target = %q, want a path inside the run", target)
				}
			}
			if got := countRequests(d.Requests(), "list "+run+"/"); got != c.wantList {
				t.Errorf("full-run listings = %d, want %d (requests: %v)", got, c.wantList, d.Requests())
			}
		})
	}
}

func TestResolveStepLogJobWithoutLog(t *testing.T) {
	d := &gcstest.Dataset{Objects: map[string][]byte{"other/x": []byte("x")}, Listing: []gcstest.Item{{Name: "other/x"}}}
	_, _, err := resolveStepLog(context.Background(), gcstest.Client(t, d), gcstest.Bucket, "other", "job")
	if err == nil || !strings.Contains(err.Error(), `no build-log.txt found for step "job"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestListArtifacts(t *testing.T) {
	const prefix = "run/artifacts/"
	d := &gcstest.Dataset{Objects: map[string][]byte{}}
	for i := 0; i < 250; i++ {
		name := prefix + fmt.Sprintf("obj-%03d", i)
		d.Objects[name] = []byte("x")
		d.Listing = append(d.Listing, gcstest.Item{Name: name, Size: "1"})
	}
	c := gcstest.Client(t, d)

	// The default asks for one extra so it can tell it is truncating.
	objs, truncated, err := listArtifacts(context.Background(), c, gcstest.Bucket, prefix, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 200 || !truncated {
		t.Fatalf("len=%d truncated=%v", len(objs), truncated)
	}

	// Unlimited reproduces the old whole-listing behaviour.
	objs, truncated, err = listArtifacts(context.Background(), c, gcstest.Bucket, prefix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 250 || truncated {
		t.Fatalf("len=%d truncated=%v", len(objs), truncated)
	}

	// A prefix with fewer objects than the limit is not truncated.
	small := &gcstest.Dataset{
		Objects: map[string][]byte{"p/a": []byte("x")},
		Listing: []gcstest.Item{{Name: "p/a", Size: "1"}},
	}
	objs, truncated, err = listArtifacts(context.Background(), gcstest.Client(t, small), gcstest.Bucket, "p/", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || truncated {
		t.Fatalf("small: len=%d truncated=%v", len(objs), truncated)
	}
}

func TestDoctorLocal(t *testing.T) {
	var buf bytes.Buffer
	doctorLocal(&buf, term.Config(new(bytes.Buffer), "never", 100), "/tmp/cache", 1<<20, 7, claudecfg.ScopeNone)
	out := buf.String()
	for _, want := range []string{"version:", "/tmp/cache (1.0 MB, 7 objects)", "stats:", "binary:", "gh:",
		"mcp:", "not registered (run: whyfail init --mcp)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// A registration names the scope it was found in; a project one is the
	// registration that applies in this directory.
	buf.Reset()
	doctorLocal(&buf, term.Config(new(bytes.Buffer), "never", 100), "/tmp/cache", 0, 0, claudecfg.ScopeProject)
	if out := buf.String(); !strings.Contains(out, "registered (project)") {
		t.Errorf("missing the project row in:\n%s", out)
	}
}

func TestDoctorClaude(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not installed", `{}`, "not installed (run whyfail init)"},
		{"plugin", `{"enabledPlugins": {"whyfail@whyfail": true}}`, "installed (whyfail@whyfail, user)"},
		{"disabled", `{"enabledPlugins": {"whyfail@whyfail": false}}`, "installed but disabled"},
		{"hooks only", `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "whyfail hook bash"}]}]}}`, "hooks-only"},
		{"duplicated", `{"enabledPlugins": {"whyfail@whyfail": true}, "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "whyfail hook bash"}]}]}}`, "duplicated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			doctorClaude(&buf, term.Config(new(bytes.Buffer), "never", 100), path)
			out := buf.String()
			if !strings.HasPrefix(out, "plugin:") {
				t.Errorf("row does not use the doctor gutter: %q", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q in %q", c.want, out)
			}
		})
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "whyfail")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameFile(real, link) {
		t.Error("a symlink to the same file must compare equal")
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if sameFile(real, other) {
		t.Error("different files compared equal")
	}
}

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"512MB", 512 << 20, false},
		{"2GB", 2 << 30, false},
		{"100KB", 100 << 10, false},
		{"4096", 4096, false},
		{"0", 0, false},
		{" 1mb ", 1 << 20, false},
		{"", 0, true},
		{"lots", 0, true},
		{"-5MB", 0, true},
	}
	for _, c := range cases {
		got, err := parseByteSize(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("parseByteSize(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
}

// TestJSONSchemas pins the contract a consumer sees: every --json document
// carries the schema key first, and the top-level fields do not silently
// disappear.
func TestJSONSchemas(t *testing.T) {
	cases := []struct {
		name string
		doc  any
		keys []string
	}{
		{"run", &run.Digest{Schema: api.SchemaV1}, nil},
		{"job", &health.Summary{Schema: api.SchemaV1}, []string{"job", "runs", "pass_rate", "verdict"}},
		{"failures", failuresOut{Schema: api.SchemaV1}, []string{"run"}},
		{"log", logOut{Schema: api.SchemaV1}, []string{"artifact", "lines", "text"}},
		{"artifact", artifactOut{Schema: api.SchemaV1},
			[]string{"object", "size", "content_type", "updated", "excerpt", "truncated"}},
		{"artifact-list", artifactListOut{Schema: api.SchemaV1, Objects: []render.ArtifactEntry{}},
			[]string{"prefix", "objects", "total_bytes", "truncated"}},
		{"search", &enrich.SearchResult{Schema: api.SchemaV1}, []string{"query", "matches"}},
		{"gain", stats.Summarize(nil, "", time.Time{}), []string{"since", "until", "totals"}},
		{"pr", prOut{Schema: api.SchemaV1}, []string{"pr", "title", "sha", "failing"}},
		{"init", initOut{Schema: api.SchemaV1}, []string{"scope", "mode", "steps", "ok"}},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.doc)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.HasPrefix(string(b), `{"schema":"`+api.SchemaV1+`"`) {
			t.Errorf("%s: schema is not the first key: %s", c.name, b[:min(len(b), 60)])
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if m["schema"] != api.SchemaV1 {
			t.Errorf("%s: schema = %v", c.name, m["schema"])
		}
		for _, k := range c.keys {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: missing top-level %q", c.name, k)
			}
		}
	}
}

// The listing document is built from the same entries as the table, so the two
// forms cannot disagree; an empty prefix is an empty array, not null.
func TestArtifactListDoc(t *testing.T) {
	doc := artifactListOut{
		Schema:     api.SchemaV1,
		Prefix:     "pr-logs/pull/org_repo/1/job/2/artifacts/",
		Objects:    []render.ArtifactEntry{{Name: "a.txt", Size: 12}},
		TotalBytes: 12,
		Truncated:  true,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Prefix  string `json:"prefix"`
		Objects []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"objects"`
		TotalBytes int64 `json:"total_bytes"`
		Truncated  bool  `json:"truncated"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Prefix != doc.Prefix || m.TotalBytes != 12 || !m.Truncated {
		t.Errorf("document = %+v", m)
	}
	if len(m.Objects) != 1 || m.Objects[0].Name != "a.txt" || m.Objects[0].Size != 12 {
		t.Errorf("objects = %+v", m.Objects)
	}

	empty, err := json.Marshal(artifactListOut{Schema: api.SchemaV1, Objects: []render.ArtifactEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"objects":[]`) {
		t.Errorf("empty listing = %s, want an empty array", empty)
	}
}

// readStats reads every record appended so far. A missing file is an empty
// ledger, not a failure: nothing has been recorded yet.
func readStats(t *testing.T) []stats.Record {
	t.Helper()
	f, err := os.Open(stats.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var recs []stats.Record
	dec := json.NewDecoder(f)
	for {
		var r stats.Record
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			return recs
		}
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// An agent looping through MCP is the highest-volume path through the tool, so
// a tool call is usage and must land in the ledger — with its target named, and
// without a byte reaching stdout, which carries JSON-RPC on this path.
func TestMCPCallRecordsStats(t *testing.T) {
	t.Setenv("WHYFAIL_CACHE_DIR", t.TempDir())
	t.Setenv("WHYFAIL_NO_STATS", "")

	const badURL = "not-a-prow-url"
	var callErr error
	out := captureStdout(t, func() {
		_, callErr = mcpCall(&globals{}, "prow_run", map[string]any{"url": badURL})
	})
	if callErr == nil {
		t.Fatal("a bad url should fail")
	}
	if out != "" {
		t.Errorf("mcpCall wrote %q to stdout", out)
	}

	recs := readStats(t)
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want exactly one", recs)
	}
	if recs[0].Cmd != "mcp/prow_run" || recs[0].Target != badURL {
		t.Errorf("record = %+v, want cmd mcp/prow_run target %q", recs[0], badURL)
	}
	// A URL that will not parse names no repository; an invented one would file
	// the call under the wrong repo in gain --by repo.
	if recs[0].Repo != "" {
		t.Errorf("repo = %q, want empty for an unparseable url", recs[0].Repo)
	}
	// A failed call is recorded at the size of its error: a retry loop against a
	// bad URL must show up rather than look free.
	if want := int64(len(callErr.Error())); recs[0].EmittedBytes != want {
		t.Errorf("emitted = %d, want %d", recs[0].EmittedBytes, want)
	}
}

func TestMCPTarget(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"prow_run", map[string]any{"url": "https://prow/view/gs/bucket/pr-logs/pull/o_r/1/job/2"}, "https://prow/view/gs/bucket/pr-logs/pull/o_r/1/job/2"},
		{"prow_job", map[string]any{"job": "pull-ci-o-r-main-unit"}, "pull-ci-o-r-main-unit"},
		// A missing or mistyped argument leaves the target empty rather than
		// inventing one, and an unknown tool has nothing to name.
		{"prow_run", map[string]any{}, ""},
		{"prow_job", map[string]any{"job": 7}, ""},
		{"other", map[string]any{"url": "x"}, ""},
	}
	for _, c := range cases {
		if got := mcpTarget(c.name, c.args); got != c.want {
			t.Errorf("mcpTarget(%q, %v) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

// The repo is derived the same way a CLI command derives it, and never guessed:
// a job name does not identify a repository, and a URL that will not parse names
// nothing at all.
func TestMCPRepo(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"prow_run", map[string]any{"url": "gs://b/pr-logs/pull/org_repo/1/job/2"}, "org/repo"},
		{"prow_run", map[string]any{"url": "https://prow.ci.openshift.org/view/gs/buk/pr-logs/pull/rh-ecosystem-edge_recert/2364/job/9"}, "rh-ecosystem-edge/recert"},
		// A periodic run has no PR path, so there is no repository to name.
		{"prow_run", map[string]any{"url": "gs://b/logs/pull-ci-org-repo-main-unit/123"}, ""},
		{"prow_run", map[string]any{"url": "not-a-prow-url"}, ""},
		{"prow_job", map[string]any{"job": "pull-ci-org-repo-main-unit"}, ""},
		{"other", map[string]any{"url": "gs://b/pr-logs/pull/org_repo/1/job/2"}, ""},
	}
	for _, c := range cases {
		if got := mcpRepo(c.name, c.args); got != c.want {
			t.Errorf("mcpRepo(%q, %v) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestMCPCallHonoursNoStats(t *testing.T) {
	t.Setenv("WHYFAIL_CACHE_DIR", t.TempDir())
	t.Setenv("WHYFAIL_NO_STATS", "")

	_, _ = mcpCall(&globals{noStats: true}, "prow_job", map[string]any{"job": "j"})
	if recs := readStats(t); len(recs) != 0 {
		t.Errorf("records = %+v, want none with --no-stats", recs)
	}
}

// A notification carries no id, so nobody can read a result for it; running it
// would spend the work and inflate the ledger with a call the agent cannot use.
func TestMCPNotificationIsNotRun(t *testing.T) {
	t.Setenv("WHYFAIL_CACHE_DIR", t.TempDir())
	t.Setenv("WHYFAIL_NO_STATS", "")

	line := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"prow_run","arguments":{"url":"not-a-prow-url"}}}`
	in, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	oldIn := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = oldIn }()

	out := captureStdout(t, func() {
		if err := serveMCP(&globals{}); err != nil {
			t.Fatalf("serveMCP: %v", err)
		}
	})
	if out != "" {
		t.Errorf("notification produced a response: %q", out)
	}
	if recs := readStats(t); len(recs) != 0 {
		t.Errorf("records = %+v, want none for a notification", recs)
	}
}

func TestCtxHonoursTimeout(t *testing.T) {
	g := &globals{timeout: 5 * time.Millisecond}
	ctx, cancel := g.ctx()
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx had no deadline")
	}
}

func TestCtxZeroTimeoutHasNoDeadline(t *testing.T) {
	g := &globals{timeout: 0}
	ctx, cancel := g.ctx()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero timeout should mean no deadline")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err() = %v, want nil", err)
	}
}

func TestTimeoutErrorMessage(t *testing.T) {
	g := &globals{timeout: 2 * time.Minute}

	if err := g.timeoutError("whyfail job", nil); err != nil {
		t.Fatalf("nil error changed into %v", err)
	}
	plain := errors.New("boom")
	if got := g.timeoutError("whyfail job", plain); got != plain {
		t.Fatalf("non-deadline error changed into %v", got)
	}
	got := g.timeoutError("whyfail job", fmt.Errorf("fetch: %w", context.DeadlineExceeded))
	if got == nil || !strings.Contains(got.Error(), "whyfail job timed out after 2m0s") {
		t.Fatalf("error %v does not name the operation and the bound", got)
	}
	if !strings.Contains(got.Error(), "--timeout") {
		t.Fatalf("error %v does not point at --timeout", got)
	}

	off := &globals{timeout: 0}
	if got := off.timeoutError("whyfail job", context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("with --timeout 0 the raw error should pass through, got %v", got)
	}
}
