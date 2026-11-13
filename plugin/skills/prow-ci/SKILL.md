---
name: prow-ci
description: Use when asked about OpenShift/Prow CI results — why a PR check failed, whether a job is flaky or broken, what a Prow run did, or how often an error appears across CI. Prefer whyfail over fetching prow.ci.openshift.org, GCS artifacts, Sippy, or search.ci directly.
---

# OpenShift CI (Prow) with whyfail

`whyfail` answers CI questions from structured data and emits compact digests.
Never curl Prow, GCS, Sippy, or search.ci directly.

## Order of operations

1. **Always start with job health** when a PR check failed:
   `whyfail job <job-name>`
   A `BROKEN` verdict means retesting cannot help; `FLAKY` means a retest is reasonable.
   Report this before any per-run detail.

2. **Then digest the run** if the user wants the failure itself:
   `whyfail run <prow-url>`
   This prints the failing step or test, a short error excerpt, infra signals
   (lease/quota/region), and a `next:` suggestion.

3. **Drill down only as needed**, and cite what you looked at:
   - `whyfail log <prow-url> <step> --tail 80`
   - `whyfail artifact <gcs-url>` (metadata plus a bounded excerpt)
   - `whyfail failures <prow-url>` (failed tests and steps only)
   - `whyfail search '<regex>'` (how many other runs hit an error)

4. **For a whole PR**: `whyfail pr <github-pr-url>` lists every failing check
   with its job health, and says outright when the PR is probably not the cause.

## Output contract

- `--json` returns the structured form (`whyfail/v1` for runs) on every command
  except `doctor`.
- The JSON digest includes counts, `monitor_causes`, and artifact inventory.
- Never claim a job "has never passed" — always read the printed window
  (`window <from> → <to>`) and the pass-rate denominator.
- Distinguish measured facts (counts, rates, windows) from modelled estimates
  (`whyfail gain` marks the latter with `~`).

## Rules

- Do not suggest `/retest` for a `BROKEN` job.
- Do not shell out to `curl` for `prow.ci.openshift.org`,
  `storage.googleapis.com/test-platform-results-public`,
  `search.ci.openshift.org`, `search.dptools.openshift.org`, or
  `sippy.dptools.openshift.org`.
- Public data only. Do not pass private or embargoed logs to external services.
- `whyfail gain` shows savings; do not fabricate savings numbers.

## Commands

```
whyfail run <prow-url|gs://bucket/path>   run digest (human or --json)
whyfail job <job-name>                    health over retained history
whyfail pr <github-pr-url>                every failing check on the head commit
whyfail failures <prow-url>               failed tests and steps only
whyfail log <prow-url> <step> [--tail N]  bounded build-log view
whyfail artifact <gcs-url> [--list]       metadata and excerpt / prefix listing
whyfail search <regex> [--max-age 48h]    search.ci match counts
whyfail gain [--history] [--by repo]      savings statistics
whyfail doctor                            connectivity and hook checks
```
