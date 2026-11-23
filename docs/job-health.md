# Job health

Where the pass rates and verdicts come from. Back to the [README](../README.md).

## Where the numbers come from

Prow keeps a public, per-job run index in its results bucket:

- presubmits: `pr-logs/directory/<job>/<run-id>.txt` (one tiny file per run)
- periodics: `logs/<job>/<run-id>/`
- each run's `finished.json` is ~450 bytes and says `SUCCESS`, `FAILURE`, or `ABORTED`

`whyfail` reads those, so it works for any repo with Prow jobs — including ones
Sippy does not track (for example `rh-ecosystem-edge/*`).

Aborted runs are counted separately (`abort`) and excluded from the pass rate and
from the "recent" rate; runs whose index entry exists but whose `finished.json` is
gone are counted as `missing` and likewise excluded. Pass rate is
`pass / (pass + fail)`. The "recent" rate is the same ratio over the last 10
completed runs.

A read that fails for a reason other than absence — a 5xx, a timeout, a decode
error — is not a fact about the run, so it is counted in `lookup_errors` and left
out of the counters entirely. A non-zero `lookup_errors` means the summary was
computed from a partial read; the card says so, and the verdict should be treated
as provisional.

Every card prints the retention window it actually used, and the run count beside a
rate describes that same window. `--window` bounds the fetch as well as the
arithmetic, so the denominator can never describe a longer period than the numbers.
See [CLI reference](cli-reference.md#flags).

## Verdicts

| Verdict | When |
|---|---|
| `UNKNOWN` | fewer than 5 completed runs in the window |
| `BROKEN` | no pass at all in the window; **or** 20+ consecutive failures; **or** no pass in the last 14 days |
| `FLAKY` | pass rate below 85%, **or** the recent rate is 10+ points below the overall rate |
| `DEGRADED` | pass rate of 85–95% |
| `HEALTHY` | pass rate of 95% and above |

`BROKEN` is the one worth reading carefully: a job can have passed inside the window
and still be `BROKEN` if it has failed 20 times since, or if its last pass is more
than two weeks old. The printed window and `last_pass_at` are the facts to quote; the
verdict is a summary of them. The order matters too — the `BROKEN` conditions are
checked before `FLAKY`, so a job with 20 consecutive failures is `BROKEN` even though
its lifetime pass rate looks healthy.

Only `BROKEN` means a retest cannot help. `FLAKY` means a retest is reasonable.
