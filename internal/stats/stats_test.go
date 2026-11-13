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
