package stats

import (
	"os"
	"testing"
	"time"
)

func TestAppendLoadSummarize(t *testing.T) {
	t.Setenv("WHYFAIL_CACHE_DIR", t.TempDir())
	t.Setenv("WHYFAIL_NO_STATS", "")

	must := func(r Record) {
		t.Helper()
		if err := Append(r); err != nil {
			t.Fatal(err)
		}
	}
	must(Record{Cmd: "run", Repo: "org/repo", ReadBytes: 1000, EmittedBytes: 100, NetworkBytes: 500, WallMS: 10, InventoryBytes: 5000})
	must(Record{Cmd: "job", Repo: "org/repo", ReadBytes: 400, EmittedBytes: 50, NetworkBytes: 400, WallMS: 5})

	// A corrupt line must be skipped, not fatal.
	f, err := os.OpenFile(Path(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	recs, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records", len(recs))
	}
	a := Summarize(recs, "cmd", time.Time{})
	if a.Totals.Calls != 2 || a.Totals.ReadBytes != 1400 || a.Totals.EmittedBytes != 150 {
		t.Fatalf("got %+v", a.Totals)
	}
	if a.Groups["run"].Calls != 1 || a.Groups["job"].Calls != 1 {
		t.Fatalf("groups: %+v", a.Groups)
	}
	if len(a.Top) != 2 || a.Top[0].Cmd != "run" {
		t.Fatalf("top: %+v", a.Top)
	}
}

func TestLoadIncludesRotatedArchive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WHYFAIL_CACHE_DIR", dir)
	t.Setenv("WHYFAIL_NO_STATS", "")

	old := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := Append(Record{TS: old, Cmd: "run", Target: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := Append(Record{TS: old.Add(time.Hour), Cmd: "job", Target: "b"}); err != nil {
		t.Fatal(err)
	}
	// Simulate what Append does at the size limit.
	if err := os.Rename(Path(), Path()+".1"); err != nil {
		t.Fatal(err)
	}
	if err := Append(Record{TS: old.Add(2 * time.Hour), Cmd: "run", Target: "c"}); err != nil {
		t.Fatal(err)
	}

	recs, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	for i, want := range []string{"a", "b", "c"} {
		if recs[i].Target != want {
			t.Errorf("record %d target = %q, want %q (archive must come first)", i, recs[i].Target, want)
		}
	}
	if a := Summarize(recs, "", time.Time{}); a.Totals.Calls != 3 {
		t.Errorf("totals = %+v", a.Totals)
	}
}

func TestLoadDeduplicatesAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WHYFAIL_CACHE_DIR", dir)
	t.Setenv("WHYFAIL_NO_STATS", "")

	dup := Record{TS: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), Cmd: "run", Target: "a"}
	if err := Append(dup); err != nil {
		t.Fatal(err)
	}
	// Copy rather than rename, so the same record sits in both files.
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path()+".1", data, 0o644); err != nil {
		t.Fatal(err)
	}

	recs, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(recs), recs)
	}
}

func TestLoadWithoutFiles(t *testing.T) {
	t.Setenv("WHYFAIL_CACHE_DIR", t.TempDir())
	recs, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if recs != nil {
		t.Fatalf("got %+v, want nil", recs)
	}
}

func TestLoadReportsUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WHYFAIL_CACHE_DIR", dir)

	// A directory where the archive should be makes the scan fail; Load must
	// surface that rather than silently reporting fewer calls.
	if err := os.Mkdir(Path()+".1", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected an error reading a directory as the archive")
	}
}

func TestDisabled(t *testing.T) {
	t.Setenv("WHYFAIL_CACHE_DIR", t.TempDir())
	t.Setenv("WHYFAIL_NO_STATS", "1")
	if err := Append(Record{Cmd: "run"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path()); !os.IsNotExist(err) {
		t.Fatalf("stats file should not exist, err=%v", err)
	}
}

func TestSummarizeByRepo(t *testing.T) {
	recs := []Record{
		{Cmd: "run", Repo: "org/repo"},
		{Cmd: "log", Repo: "org/repo"},
		{Cmd: "job"},
	}
	a := Summarize(recs, "repo", time.Time{})
	if a.Groups["org/repo"].Calls != 2 {
		t.Fatalf("repo group: %+v", a.Groups)
	}
	// A record with no repository must not land in an unlabelled blank group.
	if a.Groups["(unknown)"].Calls != 1 {
		t.Fatalf("unknown group: %+v", a.Groups)
	}
	if _, ok := a.Groups[""]; ok {
		t.Fatalf("blank group present: %+v", a.Groups)
	}
}

func TestSummarizeVerdicts(t *testing.T) {
	recs := []Record{
		{Cmd: "run", Verdict: "FLAKY"},
		{Cmd: "job", Verdict: "FLAKY"},
		{Cmd: "run", Verdict: "BROKEN"},
		{Cmd: "log"},
	}
	a := Summarize(recs, "", time.Time{})
	if a.Verdicts["FLAKY"] != 2 || a.Verdicts["BROKEN"] != 1 {
		t.Fatalf("verdicts = %+v", a.Verdicts)
	}
	if len(a.Verdicts) != 2 {
		t.Fatalf("a record without a verdict must not be counted: %+v", a.Verdicts)
	}
}

func TestSummarizeSinceFiltersTopSaves(t *testing.T) {
	now := time.Now().UTC()
	recs := []Record{
		{TS: now.Add(-48 * time.Hour), Cmd: "run", ReadBytes: 9_000_000, EmittedBytes: 1},
		{TS: now.Add(-time.Hour), Cmd: "run", ReadBytes: 5_000, EmittedBytes: 1},
	}
	a := Summarize(recs, "", now.Add(-24*time.Hour))
	if a.Totals.Calls != 1 {
		t.Fatalf("calls = %d", a.Totals.Calls)
	}
	// Top saves must describe the same window as the totals, not the whole file.
	if len(a.Top) != 1 || a.Top[0].ReadBytes != 5_000 {
		t.Fatalf("top = %+v", a.Top)
	}
	if got := len(InWindow(recs, time.Time{})); got != 2 {
		t.Fatalf("a zero cutoff must keep every record, got %d", got)
	}
}
