# whyfail

Prow CI context for humans and Claude Code.

`whyfail` answers the two questions that actually come up when OpenShift CI goes
red:

1. **Is this job broken or flaky?** — computed from Prow's own retained run
   history, across every PR that uses the job.
2. **What is in this run?** — a compact digest of the failing step or test, the
   error, and the infrastructure signals, instead of megabytes of GCS artifacts.

No authentication, no LLM, no automatic retests.

## Install

```sh
make build          # ./bin/whyfail
make install        # ~/go/bin/whyfail
```

Requires Go 1.27+ (matching `go.mod`). The `pr` command additionally needs the `gh`
CLI.

## Use

```sh
# Is the job itself healthy? (fastest, most useful)
whyfail job pull-ci-rh-ecosystem-edge-recert-main-e2e-aws-ovn-single-node-recert-serial

# What happened in one run?
whyfail run <prow-url>

# Every failing check on a PR, with job health
whyfail pr https://github.com/rh-ecosystem-edge/recert/pull/2364

# Failed tests and steps only
whyfail failures <prow-url>

# Bounded log / artifact access
whyfail log <prow-url> <step> --tail 80
whyfail artifact <gcs-url>

# How often has this error been seen elsewhere?
whyfail search 'Platform Prometheus targets should not be accessible'
```

Every command accepts `--json` (`whyfail/v1` for runs) except `doctor`, which always
prints its checklist as text.

### Flags

Global flags apply to every command:

| Flag | Meaning |
|---|---|
| `--json` | structured output instead of human text |
| `--no-stats` | do not record this invocation in `stats.jsonl` |
| `--cache-dir <dir>` | object-cache location (default: the user cache dir) |
| `--refresh` | ignore cached listings and objects |
| `--window <dur>` | history window, e.g. `720h`; `0` = all retained runs |
| `--bucket <name>` | GCS bucket used for job history |
| `--bytes-per-token <n>` | bytes-per-token estimate used by `gain` |
| `--baseline <dur>` | modelled manual time per call used by `gain` |
| `--color auto\|always\|never` | colourise human output (default `auto`: only on a TTY) |
| `--width <n>` | output width in columns (default `$COLUMNS`, else 100) |

Command-specific flags:

- `run` — `--no-history` (skip the job-health lookup), `--search` (look up the top
  failure signature on search.ci), `--no-sippy` (skip the Sippy coverage probe)
- `log` — `--tail <n>` (trailing lines, default 120)
- `artifact` — `--list` (list a prefix instead of reading an object)
- `search` — `--max-age <dur>` (search window, default `48h`)
- `gain` — `--history` (recent calls), `--since <dur>`, `--by cmd|repo`
  (aliases: `stats`, `saved`)

### Environment

- `WHYFAIL_CACHE_DIR` — relocates both the object cache and `stats.jsonl`
- `WHYFAIL_NO_STATS` — set to anything to disable statistics recording
- `NO_COLOR` / `CLICOLOR=0` / `TERM=dumb` — disable colour, as `--color never` does

### Output

Human output is one card with a fixed 9-character label gutter, colour-coded
verdicts, and everything folded to the terminal width; `--json` stays structured:

```
pull-ci-rh-ecosystem-edge-recert-main-baremetalds-sno-recert-cluster-rename
⚠ FLAKY     63.8% pass · 154 runs since 2026-09-17 · last pass 2026-11-12

  RUN       2120987667142283264   FAILURE   2h13m   28% of 8h00m timeout
  PR        rh-ecosystem-edge/recert#2364   sebrandon1
            "CNF-30065: add integration scenarios for cluster customizations…"
  CLUSTER   build10 · ci · /test baremetalds-sno-recert-cluster-rename
  REASON    executing_graph:step_failed:utilizing_lease:executing_test:executing_…

  ────────────────────────────────────────────────────────────────────────
  STEPS     13 total · 3 failed
  ✗  baremetalds-sno-recert-cluster-rename    1h51m
  ✗  baremetalds-sno-test                    12m11s
       no JUnit report to /tmp/artifacts/reports/e2e-monitor-tests__…xml
  ✗  gather-audit-logs                           2s
       error getting cluster version: tls: failed to verify certificate…
       … (2 more lines in --json)

  TESTS     50 pass · 0 fail · 0 skip   [extension]
  MONITORS  76 failures · 8 causes
   51×  monitor: kubelet-container-restarts
   13×  monitor: required-scc-annotation-checker

  NEXT      a retest is reasonable
```

Paths are shortened from the middle so the filename survives, and long errors are
cut to two lines — `--json` and `whyfail log` carry the rest.

## How the health numbers are computed

Prow keeps a public, per-job run index in its results bucket:

- presubmits: `pr-logs/directory/<job>/<run-id>.txt` (one tiny file per run)
- periodics: `logs/<job>/<run-id>/`
- each run's `finished.json` is ~450 bytes and says `SUCCESS`, `FAILURE`, or `ABORTED`

`whyfail` reads those, so it works for any repo with Prow jobs — including ones
Sippy does not track (for example `rh-ecosystem-edge/*`). Aborted runs are
reported separately and excluded from the pass rate, and every card prints the
retention window it actually used.

Verdicts: `BROKEN` (never passes in the window), `FLAKY` (<85% or a recent
drop), `DEGRADED` (85–95%), `HEALTHY` (≥95%), `UNKNOWN` (<5 runs).

## Run digests

The digest uses structured sources first:

- `prowjob.json` — PR, author, title, base SHA, rerun command, timeout
- `artifacts/ci-operator-step-graph.json` — per-step durations and failures
- `artifacts/ci-operator-metrics.json` — cloud lease, quota slice, machines,
  cluster profile, ci-operator completion
- `extension_test_result_*.json` — per-test `lifecycle` (blocking/informing),
  result, duration and error (far better than parsing multi-megabyte JUnit)
- JUnit files — `junit_e2e__*.xml`, `junit_install.xml`, and a bare `junit.xml`,
  used when a job emits no extension result JSON. Pass/fail/skip counts are derived
  from the `<testcase>`/`<failure>`/`<error>`/`<skipped>` elements, and `<error>` is
  treated like `<failure>`
- `e2e-monitor-tests` — monitor failures, grouped into causes
- `sippy=tracked` / `not tracked` — whether Sippy has coverage for the job at all

Monitor failures are collapsed by cause, so “76 failures” becomes “8 causes” with
the dominant one first.

## Claude Code plugin

`plugin/` is a Claude Code plugin that installs a `PreToolUse` hook and a skill:

- a bare `curl` or `wget` of a single Prow run URL is rewritten to
  `whyfail run <url>`
- a bare `curl` or `wget` of a GCS artifact URL is rewritten to
  `whyfail artifact <url>`
- a `search.ci.openshift.org` or `search.dptools.openshift.org` URL is rewritten to
  `whyfail search '<query>'`
- `WebFetch` on those hosts is denied with the equivalent whyfail command
- the `prow-ci` skill tells Claude to check job health before run detail

Only a single-URL fetch with no pipe, redirection, or extra argument is rewritten;
anything more complex is left untouched.

Install the plugin with your Claude Code plugin manager and make sure `whyfail` is on
`PATH`. `whyfail doctor` checks the binary, cache, stats and endpoint reachability —
it does not verify that the plugin itself is installed.

## Savings

Every invocation is recorded locally (`~/.cache/whyfail/stats.jsonl`):

```sh
whyfail gain
whyfail gain --history
whyfail gain --by repo --since 7d
```

It reports bytes read versus emitted, estimated tokens saved, and separates
measured numbers from modelled ones. Disable with `--no-stats` or
`WHYFAIL_NO_STATS=1`.

## Limitations

- History covers Prow's GCS retention (roughly 60–90 days), not all time.
- Public data only; private/embargoed repositories are out of scope.
- Sippy is used only when it has coverage; for many repos the correct answer is
  `not tracked`.
- The `pr` command depends on the GitHub API through `gh`.

## Development

```sh
make test       # hermetic: unit tests + recorded Prow fixtures
make lint
make test-live  # real network: four digests (3 recert + cert-manager), health, and savings
make fixtures   # re-record both testdata corpora from live Prow
```

Every push to `main` and every pull request runs `gofmt`, `go vet`,
`golangci-lint`, `go test ./...` and `make build` in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml). `make test-live` stays
manual — it reads the real Prow bucket.

`testdata/` holds public artifacts recorded by `scripts/fetch-fixtures.sh`:
three failing runs from
[rh-ecosystem-edge/recert#2364](https://github.com/rh-ecosystem-edge/recert/pull/2364)
(extension-JSON test results, `sippy=not tracked`) and one from
[openshift/cert-manager-operator#533](https://github.com/openshift/cert-manager-operator/pull/533)
(a bare `artifacts/junit.xml`, `sippy=tracked`). Bodies are stored exactly as GCS stores
them, so the sizes in `listing.json.gz` and the on-disk sizes agree and
`Artifacts.TotalBytes` stays truthful. The hermetic tests serve them through
`internal/gcstest`; the `live` build tag runs the same assertions against the
real bucket, so Prow layout changes surface as a failure naming the run.
