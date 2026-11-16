# Run digests

What `whyfail run` reads, and why it can answer without the Prow page.
Back to the [README](../README.md).

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

Monitor failures are collapsed by cause, so "76 failures" becomes "8 causes" with
the dominant one first.

A digest is bounded: long errors are cut to two lines and paths are shortened from
the middle. `--json` and `whyfail log` carry the rest, so a digest never pulls a
multi-megabyte artifact into a conversation. See
[JSON output](json-output.md) for the shape and
[CLI reference](cli-reference.md) for `log` and `artifact`.
