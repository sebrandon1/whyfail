# The flaky-job board

A public page that ranks the Prow presubmit jobs you care about, refreshed once a
day. It answers "which jobs are red right now?" without one CLI call per job, and
it keeps a trend past Prow's ~60–90 day retention.

## What produces it

`whyfail report` collects health for every job in a curated list and writes one
`whyfail/v1` document:

```sh
whyfail report --jobs report/jobs.txt --window 720h --out snapshot.json
```

Each row is the same summary `whyfail job <name>` prints — verdict, pass rate,
recent rate, run counts, fail streak, distinct failing PRs — plus the repository
(derived offline from the run paths) and the job's Prow history URL. Rows are
sorted worst first: verdict severity, then a regression, then failing runs, then
name.

A job in the list that matches nothing in the bucket becomes an `UNKNOWN` row
rather than an error, and a lookup that fails carries `error` on the row, so one
flaky bucket request cannot blank the board.

### Since the last point

When `--history` is given, each row is compared with the newest point in that
file *strictly before today*, so a same-day re-run compares against yesterday
rather than against its own earlier attempt. A row that got worse carries
`regressed: true` and its `previous_verdict`; the page shows a "newly broken"
count and a `was <verdict>` marker beside the row. `UNKNOWN` on either side is
not compared — it means there was no signal, not that the job improved or broke.

Without `--history` both fields are omitted and the snapshot is exactly what it
was before.

## What publishes it

`.github/workflows/pages.yml` runs at 05:17 UTC and on `workflow_dispatch`:

1. builds `whyfail`,
2. restores `history.json` from the `gh-pages` branch,
3. runs `whyfail report` (which merges today's point into that history and keeps
   180 days),
4. refuses to publish an empty board, a board where every job is `UNKNOWN`, or a
   board with a failed lookup,
5. force-pushes the site and the two JSON files to `gh-pages`.

The workflow does not commit to `main`, so branch protection never gets in the
way and `main` never carries generated output. The site is `site/` in this
repository: `index.html`, `app.js`, `style.css` — no build step, no dependencies,
and no Go module change.

**One-time setup:** Settings → Pages → Source: *Deploy from a branch*, branch
`gh-pages`, folder `/ (root)`.

## Changing the list

Edit `report/jobs.txt` — one presubmit job name per line, `#` for comments — and
commit. The next daily run picks it up; `workflow_dispatch` runs it now.

The list is curated on purpose: `pr-logs/directory/` holds thousands of jobs
across all of OpenShift CI, and walking it daily costs many list requests for
rows nobody reads.

## The files on the published branch

| File | Contents |
|---|---|
| `index.html`, `app.js`, `style.css` | the board |
| `snapshot.json` | today's `whyfail report` output |
| `history.json` | one point per job per day: verdict, pass rate, runs |

A re-run on the same day replaces that day's point, so the chart does not gain a
spike every time the workflow is retried.

## Limits

- One snapshot a day; a job that breaks and is fixed inside a day shows once.
- Per-test flakiness is a different source and is not on the board.
- No authentication, and nothing private: it reads the public
  `test-platform-results-public` bucket only.
