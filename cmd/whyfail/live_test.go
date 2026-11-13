//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sebrandon1/whyfail/internal/gcstest"
	"github.com/sebrandon1/whyfail/internal/stats"
)

// This is the only place the savings ledger can be tested end to end: stats are
// recorded in globals.emit, so the real binary has to run.

const (
	ipcURL    = "https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/rh-ecosystem-edge_recert/2364/pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow/2120987688944275456"
	ipcTarget = "pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow/2120987688944275456"
	ipcInv    = int64(24052955) // listing bytes for the run
	ipcLog    = int64(16029)    // uncompressed build-log.txt, the only uncached read
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "whyfail-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "whyfail")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/whyfail")
		cmd.Dir = gcstest.RepoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build whyfail: %v", buildErr)
	}
	return binPath
}

func runCLI(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("whyfail %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func TestLiveSavingsColdWarm(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("WHYFAIL_CACHE_DIR", cache)
	t.Setenv("WHYFAIL_NO_STATS", "")

	cold := mustRun(t, "run", ipcURL, "--no-sippy")
	warm := mustRun(t, "run", ipcURL, "--no-sippy")

	if cold != warm {
		t.Errorf("warm output differs from cold (%d vs %d bytes)", len(cold), len(warm))
	}
	if len(cold) > 8*1024 {
		t.Errorf("digest is %d bytes, want under 8 KiB", len(cold))
	}
	for _, want := range []string{"run 2120987688944275456", "reason:", "next:", "url:"} {
		if !strings.Contains(cold, want) {
			t.Errorf("digest missing %q", want)
		}
	}

	recs := loadRecords(t)
	if len(recs) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(recs))
	}
	coldRec, warmRec := recs[0], recs[1]

	for i, r := range recs {
		if r.Cmd != "run" || r.Target != ipcTarget || r.Repo != "rh-ecosystem-edge/recert" {
			t.Errorf("record[%d] identity = %q/%q/%q", i, r.Cmd, r.Target, r.Repo)
		}
		if r.InventoryBytes != ipcInv {
			t.Errorf("record[%d] inventory = %d, want %d", i, r.InventoryBytes, ipcInv)
		}
		if r.EmittedBytes != int64(len(cold)) {
			t.Errorf("record[%d] emitted = %d, want len(stdout) = %d", i, r.EmittedBytes, len(cold))
		}
		if r.ReadBytes <= r.EmittedBytes {
			t.Errorf("record[%d] read %d is not larger than emitted %d", i, r.ReadBytes, r.EmittedBytes)
		}
	}

	// The only warm byte in a cold run is the run's own finished.json: Build
	// fetches it, then the history scan asks for the same object again.
	const finishedJSON = int64(452)
	if coldRec.CacheHits != 1 {
		t.Errorf("cold cache hits = %d, want 1 (the run's own finished.json)", coldRec.CacheHits)
	}
	if diff := coldRec.ReadBytes - coldRec.NetworkBytes; diff != coldRec.CacheHits*finishedJSON {
		t.Errorf("cold served-network difference = %d, want %d (%d hits x %d bytes)",
			diff, coldRec.CacheHits*finishedJSON, coldRec.CacheHits, finishedJSON)
	}

	if warmRec.CacheHits == 0 {
		t.Error("warm run recorded no cache hits")
	}
	if warmRec.ReadBytes != coldRec.ReadBytes {
		t.Errorf("warm read %d != cold read %d; the same objects should be served", warmRec.ReadBytes, coldRec.ReadBytes)
	}
	// build-log.txt goes through GetLimit, which never caches, so a warm run
	// still downloads exactly that file and nothing else.
	if warmRec.NetworkBytes != ipcLog {
		t.Errorf("warm network = %d, want %d (the uncached build log)", warmRec.NetworkBytes, ipcLog)
	}

	// The headline claim: a few KB out of a 24 MB tree, reading a few MB.
	if coldRec.ReadBytes >= coldRec.InventoryBytes {
		t.Errorf("read %d should be well under the %d byte tree", coldRec.ReadBytes, coldRec.InventoryBytes)
	}
	if coldRec.ReadBytes < 100*coldRec.EmittedBytes {
		t.Errorf("compression weaker than expected: read %d, emitted %d", coldRec.ReadBytes, coldRec.EmittedBytes)
	}
}

func TestLiveSavingsOptOut(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("WHYFAIL_CACHE_DIR", cache)

	mustRun(t, "run", ipcURL, "--no-sippy", "--no-stats")
	mustRun(t, "run", ipcURL, "--no-sippy", "--no-stats")

	if recs := loadRecords(t); len(recs) != 0 {
		t.Fatalf("--no-stats recorded %d calls", len(recs))
	}

	// The env var suppresses recording even when the flag is absent.
	t.Setenv("WHYFAIL_NO_STATS", "1")
	mustRun(t, "run", ipcURL, "--no-sippy")
	if recs := loadRecords(t); len(recs) != 0 {
		t.Fatalf("WHYFAIL_NO_STATS recorded %d calls", len(recs))
	}

	// Suppression must not disable caching: re-enable and run again.
	t.Setenv("WHYFAIL_NO_STATS", "")
	rec := runOne(t, "run", ipcURL, "--no-sippy")
	if rec.CacheHits == 0 {
		t.Error("a recorded run after opt-out runs should still hit the cache")
	}
}

func TestLiveGainAggregate(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("WHYFAIL_CACHE_DIR", cache)
	t.Setenv("WHYFAIL_NO_STATS", "")

	mustRun(t, "run", ipcURL, "--no-sippy")
	mustRun(t, "run", ipcURL, "--no-sippy")
	recs := loadRecords(t)
	if len(recs) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(recs))
	}

	var sum struct {
		Totals stats.Totals `json:"totals"`
	}
	out := mustRun(t, "gain", "--json")
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("gain --json: %v\n%s", err, out)
	}

	wantRead := recs[0].ReadBytes + recs[1].ReadBytes
	wantEmitted := recs[0].EmittedBytes + recs[1].EmittedBytes
	wantNet := recs[0].NetworkBytes + recs[1].NetworkBytes
	if sum.Totals.Calls != 2 {
		t.Errorf("calls = %d, want 2", sum.Totals.Calls)
	}
	if sum.Totals.ReadBytes != wantRead {
		t.Errorf("read = %d, want %d", sum.Totals.ReadBytes, wantRead)
	}
	if sum.Totals.EmittedBytes != wantEmitted {
		t.Errorf("emitted = %d, want %d", sum.Totals.EmittedBytes, wantEmitted)
	}
	if sum.Totals.NetworkBytes != wantNet {
		t.Errorf("network = %d, want %d", sum.Totals.NetworkBytes, wantNet)
	}

	human := mustRun(t, "gain")
	for _, want := range []string{"whyfail saved", "~tokens saved", "full trees"} {
		if !strings.Contains(human, want) {
			t.Errorf("gain output missing %q\n%s", want, human)
		}
	}
	if grouped := mustRun(t, "gain", "--by", "repo"); !strings.Contains(grouped, "rh-ecosystem-edge/recert") {
		t.Errorf("gain --by repo missing the repo\n%s", grouped)
	}

	// Running gain must not itself be recorded.
	if recs := loadRecords(t); len(recs) != 2 {
		t.Errorf("gain recorded %d new calls", len(recs)-2)
	}
}

func runOne(t *testing.T, args ...string) stats.Record {
	t.Helper()
	before := len(loadRecords(t))
	mustRun(t, args...)
	recs := loadRecords(t)
	if len(recs) != before+1 {
		t.Fatalf("%s recorded %d calls", strings.Join(args, " "), len(recs)-before)
	}
	return recs[len(recs)-1]
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	return runCLI(t, nil, args...)
}

func loadRecords(t *testing.T) []stats.Record {
	t.Helper()
	recs, err := stats.Load() // returns nil, nil when nothing has been recorded
	if err != nil {
		t.Fatalf("load stats: %v", err)
	}
	return recs
}
