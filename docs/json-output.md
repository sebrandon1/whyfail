# JSON output

One schema for every command. Back to the [README](../README.md).

Every `--json` document is one named shape whose first key is
`"schema": "whyfail/v1"`, so a consumer can assert one thing across all commands:

```sh
whyfail <cmd> --json | jq -e '.schema == "whyfail/v1"'
```

Fields are additive: a name is never reused for something else. New fields appear;
existing ones keep their meaning.

| Command | Top-level keys |
|---|---|
| `run` | `schema`, `job`, `run`, `tests`, `infra`, `artifacts`, `signals`, `next_action` |
| `job` | `schema`, `job`, `from`, `to`, `runs`, `pass`, `fail`, `abort`, `missing`, `pass_rate`, `recent_pass_rate`, `streak`, `last_pass_at`, `distinct_prs_failing`, `verdict` |
| `failures` | `schema`, `run`, `tests`, `failed_steps` |
| `log` | `schema`, `artifact`, `lines`, `text` |
| `artifact` | `schema`, `object`, `size`, `content_type`, `updated`, `excerpt`, `truncated` |
| `search` | `schema`, `query`, `matches`, `jobs`, `known_bug` |
| `gain` | `schema`, `since`, `until`, `totals`, `groups`, `top_saves`, `verdicts` |
| `pr` | `schema`, `pr`, `title`, `sha`, `failing[]` — each card is `check`, `job`, `prow`, `health` |
| `init` | `schema`, `scope`, `mode`, `dry_run`, `from`, `settings`, `steps[]`, `ok` |

`doctor` has no JSON form; it prints its checklist as text.

Sections that only exist sometimes are omitted rather than emitted empty: `job`,
`tests`, `infra`, `artifacts` and `signals` are absent when the underlying data was not
available, and `job`'s `health` object is where the details live for a `run` digest.

## Health semantics

A `pr` card's `health` is `null` when there is no Prow job behind the check
(`prow: false`) or when the job is not in the bucket; neither case is reported as
healthy. See [Job health](job-health.md) for what each verdict means.

`job`'s per-run sample list is an internal detail and is not part of the JSON document;
the aggregate counters are. `run` nests failed steps under `run.steps.failed`, while
`failures` lists them at `failed_steps` — both names are part of v1.
