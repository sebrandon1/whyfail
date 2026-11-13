---
description: Digest a Prow job run (failing step or test, error excerpt, infra signals)
argument-hint: <prow-url>
allowed-tools: Bash(whyfail:*)
---

Run `whyfail run $ARGUMENTS` and summarize:

- the run result and duration versus its timeout,
- any failing step or blocking test, with the one-line error,
- the infrastructure signals (lease region/slice, install failure),
- the `next:` recommendation.

If a job-health verdict is present, lead with it. Do not fetch the Prow page or
GCS artifacts separately.
