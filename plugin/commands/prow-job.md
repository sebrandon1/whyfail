---
description: Job health over retained Prow history (broken / flaky / healthy)
argument-hint: <job-name>
allowed-tools: Bash(whyfail:*)
---

Run `whyfail job $ARGUMENTS` and report:

- verdict, pass rate, and the printed window (never imply all-time history),
- run counts (pass / fail / abort) and the last pass date,
- how many distinct PRs are failing, which shows whether the job is red for everyone,
- the recommended action: do not retest a `BROKEN` job; a retest is reasonable for `FLAKY`.
