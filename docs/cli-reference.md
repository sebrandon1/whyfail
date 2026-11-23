# CLI reference

Commands, flags, environment variables, output format and caching.
Back to the [README](../README.md).

## Commands

```sh
whyfail run <prow-url | gs://bucket/path>   # digest one run
whyfail job <job-name>                      # health over retained history
whyfail pr <github-pr-url | owner/repo#N | N --repo owner/repo>  # every failing check on the head commit
whyfail failures <prow-url>                 # failed tests and steps only
whyfail log <prow-url> <step> [--tail N] [--grep RE]   # bounded build-log view
whyfail artifact <gcs-url>                  # metadata and excerpt, or --list a prefix
whyfail search <regex>                      # how often an error appears in CI
whyfail report --jobs report/jobs.txt       # health for a curated job list, as one document
whyfail gain                                # savings statistics
whyfail init                                # install the Claude Code integration
whyfail doctor                              # binary, cache, stats, endpoint checks
```

Two more commands are deliberately hidden: `hook` is the Claude Code `PreToolUse`
entrypoint and `mcp` is the stdio server. Neither is meant to be typed by hand; see
[Claude Code integration](claude-code.md). `completion` and `help` are cobra's own.

Every command that emits a document accepts `--json` — all of the above except
`doctor`, which always prints its checklist as text. Every document carries
`"schema": "whyfail/v1"` as its first key; see [JSON output](json-output.md).

## Flags

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
| `--retries <n>` | retry transient HTTP failures (429, 5xx) `n` times (default 2) |
| `--timeout <dur>` | overall deadline for a command, e.g. `30s` or `5m`; `0` disables (default 2m) |
| `--cache-max <size>` | object-cache limit, e.g. `512MB` or `2GB`; `0` disables pruning (default 512MB) |
| `--color auto\|always\|never` | colourise human output (default `auto`: only on a TTY) |
| `--width <n>` | output width in columns (default `$COLUMNS`, else 100) |

Command-specific flags:

- `run` — `--no-history` (skip the job-health lookup), `--search` (look up the top
  failure signature on search.ci), `--no-sippy` (skip the Sippy coverage probe)
- `log` — `--tail <n>` (trailing lines, default 120; with `--grep`, the maximum
  matches printed), `--grep <regex>` (only matching lines, numbered), `--context <n>`
  (lines of context around each match, merged so nothing prints twice)
- `artifact` — `--list` (list a prefix instead of reading an object), `--limit <n>`
  (maximum objects to list, default 200; `0` = all)
- `search` — `--max-age <dur>` (search window, default `48h`)
- `pr` — `--repo <owner/repo>` (repository for a bare PR number; a full URL and
  `owner/repo#N` need no flag)
- `gain` — `--history` (recent calls), `--fun` (the less serious statistics),
  `--since <dur>`, `--by cmd|repo` (aliases: `stats`, `saved`)
- `init` — `--global` (install for the user, the default), `--project` (install for the
  current repository instead of the user), `--hooks-only` (write the hooks into
  `settings.json` instead of using the plugin system), `--mcp` (also register the MCP
  server), `--from <owner/repo|url|path>` (marketplace source, default
  `sebrandon1/whyfail`), `--dry-run`, `--uninstall`, `-y/--yes`
- `report` — `--jobs <file>` (curated list, default `report/jobs.txt`), `--out <file>`
  (snapshot destination; default stdout), `--history <file>` (merge today's point into
  this history file), `--keep <n>` (days of history to retain, default 180)

## Environment

- `WHYFAIL_CACHE_DIR` — relocates both the object cache and `stats.jsonl`
- `WHYFAIL_NO_STATS` — set to anything to disable statistics recording
- `NO_COLOR` / `CLICOLOR=0` / `TERM=dumb` — disable colour, as `--color never` does

## Output

Human output is one card with a fixed 9-character label gutter, colour-coded
verdicts, and everything folded to the terminal width; `--json` stays structured.
Every human path uses the same gutter — `run`, `job`, `failures`, `pr`, `gain`,
`gain --history`, `search`, `artifact` and `doctor` — so `--width` and `--color`
apply uniformly:

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

### Exit codes

`0` on success, `1` on any error. A failed CI check is data, not an error: `whyfail
job` and `whyfail pr` exit `0` while reporting `BROKEN` or `FLAKY`.

## Cache

Fetched objects are cached under `--cache-dir`; listings never are. The cache is
pruned at most once a day, oldest object first, until it is under `--cache-max` —
and never down to objects written in the last hour, so a running command cannot
lose a file it just wrote. `--refresh` neither reads nor prunes the cache, so
evidence of a bad cache survives.

## doctor

`whyfail doctor` checks what a failing command would otherwise reveal the hard way.
It prints one row per check:

| Row | What it means |
|---|---|
| `version` | the build's version, from `git describe` |
| `cache` | cache directory, size and object count |
| `stats` | `stats.jsonl` path and whether recording is enabled |
| `binary` | the `whyfail` on `PATH`, flagged when it is not the binary you ran |
| `gh` | the GitHub CLI `whyfail pr` needs |
| `mcp` | whether the MCP server is registered, and in which scope, read from the scope's own file rather than `claude mcp get` |
| `plugin` | Claude Code integration state, read from `settings.json` |
| `gcs` | a listing request against the Prow results bucket |
| `sippy` | Sippy coverage probe |
| `search.ci` | a search.ci request |

`doctor` reads no artifacts, so it records no statistics and cannot inflate `gain`.
See [Claude Code integration](claude-code.md) for the `plugin` row's values.
