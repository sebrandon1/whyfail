---
description: Health of every failing Prow check on a PR's head commit
argument-hint: <github-pr-url>
allowed-tools: Bash(whyfail:*)
---

Run `whyfail pr $ARGUMENTS` and report each failing job with its health verdict
and pass rate. If every failing job is `BROKEN` or `FLAKY`, say plainly that the
PR is probably not the cause. Requires the `gh` CLI for GitHub access.
