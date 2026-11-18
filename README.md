# whyfail

[![CI](https://github.com/sebrandon1/whyfail/actions/workflows/ci.yml/badge.svg)](https://github.com/sebrandon1/whyfail/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/sebrandon1/whyfail)](https://github.com/sebrandon1/whyfail/blob/main/go.mod)
[![License](https://img.shields.io/github/license/sebrandon1/whyfail)](LICENSE)

Prow CI context for humans and Claude Code.

`whyfail` answers the two questions that actually come up when OpenShift CI goes red:
**is this job broken or flaky?** — from Prow's own retained run history, across every
PR that uses the job — and **what is in this run?** — a compact digest of the failing
step or test, the error, and the infrastructure signals, instead of megabytes of GCS
artifacts. No authentication, no LLM, no automatic retests.

## Features

- **Job verdicts** — `BROKEN`, `FLAKY`, `DEGRADED`, `HEALTHY` over a printed retention
  window, with pass rate, last pass, and how many PRs the job is failing for
- **Run digests** — the failing step or blocking test, a bounded error excerpt, cloud
  lease and install signals, and a `NEXT` recommendation
- **PR triage** — every failing check on the head commit, Prow and GitHub, each with
  its job health, so "this PR broke it" is distinguishable from "this job is red"
- **Bounded reads** — `log` and `artifact` return an excerpt, never a multi-megabyte file
- **`--json` everywhere** — one `whyfail/v1` document per reporting command
- **Claude Code integration** — hooks, slash commands, a skill, and an MCP server

## Install

```sh
go install github.com/sebrandon1/whyfail/cmd/whyfail@latest

# or from a checkout
make build          # ./bin/whyfail
make install        # ~/go/bin/whyfail
whyfail init        # Claude Code integration (plugin, hooks, skill)
```

Requires Go 1.27+ (matching `go.mod`). The `pr` command needs the `gh` CLI, and
`init` needs the `claude` CLI unless you pass `--hooks-only`.

## Quick start

```sh
whyfail job pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow   # broken or flaky?
whyfail run <prow-url>          # what happened in one run?
whyfail pr <github-pr-url>      # every failing check, with job health
whyfail failures <prow-url>     # failed tests and steps only
whyfail log <prow-url> <step> --tail 80
whyfail artifact <gcs-url>      # metadata and excerpt
whyfail search '<regex>'        # how often has this error been seen?
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | the command ran, whatever it found |
| 1 | the tool failed — bad URL, network error, timeout |
| 2 | `run --exit-code`: the run did not pass |

A failed run is a result, so it only changes the exit status when you ask for that
with `--exit-code`; the digest still prints in full.

## Guides

| Guide | Contents |
|---|---|
| [CLI reference](docs/cli-reference.md) | Every command, flag, environment variable, output format and cache rule |
| [JSON output](docs/json-output.md) | The `whyfail/v1` schema, per-command keys, health semantics |
| [Flaky-job board](docs/report.md) | The daily dashboard: `whyfail report`, the curated job list, the publishing workflow |
| [Job health](docs/job-health.md) | Where pass rates come from and what each verdict means |
| [Run digests](docs/run-digests.md) | The structured artifacts a digest reads, and why it is bounded |
| [Claude Code integration](docs/claude-code.md) | Hooks, slash commands, the `prow-ci` skill, `init`, and the MCP server |
| [Savings](docs/savings.md) | How `gain` measures bytes and models time, and how to disable stats |
| [Development](docs/development.md) | Build targets, CI gates, and the recorded Prow fixtures |

## Limitations

- History covers Prow's GCS retention (roughly 60–90 days), not all time.
- Public data only; private/embargoed repositories are out of scope.
- Sippy is used only when it has coverage; for many repos the answer is `not tracked`.
- The `pr` command depends on the GitHub API through `gh`.

## Development

`make test` is hermetic (unit tests plus recorded Prow fixtures); `make lint` and
`make build` cover the rest. Every push and pull request runs `gofmt`, `go vet`,
`golangci-lint`, a `go mod tidy` diff gate, `go test ./...` and `make build`. See
[Development](docs/development.md) for the fixture corpus and the live tests.

Licensed under Apache-2.0; see [LICENSE](LICENSE).
