# Claude Code integration

The plugin, the hooks, and the MCP server. Back to the [README](../README.md).

`whyfail` ships a Claude Code plugin, in `plugin/`, that keeps Claude from scraping
Prow pages and multi-megabyte GCS artifacts. It installs three things:

- **two `PreToolUse` hooks** — one for `Bash`, one for `WebFetch`
- **three slash commands** — `/prow-run`, `/prow-job`, `/prow-pr`
- **one skill** — `prow-ci`, which tells Claude to check job health before run detail

## What the hooks do

On `Bash`:

- a bare `curl` or `wget` of a single Prow run URL is rewritten to `whyfail run <url>`
- a bare `curl` or `wget` of a GCS artifact URL is rewritten to `whyfail artifact <url>`
- a bare `curl` or `wget` of a `search.ci.openshift.org` or
  `search.dptools.openshift.org` URL with a `?search=` query is rewritten to
  `whyfail search '<query>'`

Only a single-URL fetch with no pipe, redirection, or extra argument is rewritten;
anything more complex is left untouched. A command that already mentions `whyfail` is
always allowed through.

On `WebFetch`, a URL that maps to a whyfail command is denied with the equivalent
command as the reason. For `search.*` that only applies when the URL carries a
`?search=` query; a search host URL without one is not something whyfail can replace,
so it is allowed.

## Installing

One command installs it:

```sh
whyfail init                       # plugin via the claude CLI (marketplace + install)
whyfail init --hooks-only          # no plugin system: settings.json, commands and skill
whyfail init --mcp                 # also register the MCP server
whyfail init --dry-run             # print the steps and change nothing
whyfail init --uninstall           # reverse whatever init did
```

The plugin path is the default because the plugin manager owns updates and
enable/disable. `--hooks-only` exists for anyone who would rather not use the plugin
system: it appends the same two `PreToolUse` entries to `settings.json` — leaving every
other key and hook untouched — and copies the commands and skill into `~/.claude`. It
refuses when the plugin is already enabled, so the hooks never run twice. Both paths
need `whyfail` on `PATH`, because the hooks execute it directly.

`--project` installs into `./.claude` instead of the user scope, and `--from` overrides
the marketplace source (default `sebrandon1/whyfail`). Nothing `init` does is recorded
in the savings statistics.

## Doctor's plugin row

`whyfail doctor` reports which of these is in place, by reading `settings.json`, so it
stays offline and never shells out to `claude`:

| Row value | Meaning |
|---|---|
| `installed (whyfail@whyfail, user)` | plugin enabled |
| `installed but disabled (run: claude plugin enable whyfail@whyfail)` | plugin known, not enabled |
| `hooks-only` | whyfail hook present in `PreToolUse` |
| `duplicated: plugin and settings.json hooks (hooks run twice)` | both paths installed — uninstall one |
| `not installed (run whyfail init)` | neither |

## MCP server

`whyfail mcp` is a stdio MCP server exposing typed tools — useful when a client
would rather call a function than be handed a rewritten shell command:

| Tool | Arguments | Returns |
|---|---|---|
| `prow_run` | `url` — a Prow view URL or `gs://` reference | the run digest |
| `prow_job` | `job` — a job name | health over retained history |
| `prow_pr` | `pr` — a GitHub PR URL | every failing check on the head commit, with job health (needs `gh`) |
| `prow_search` | `query` — a regex; `max_age` — search window (default `48h`) | how often a signature appears on search.ci |
| `prow_log` | `url`, `step`; `tail` (default 120), `grep` | a bounded view of a step's build log |

Register it with `whyfail init --mcp`, or by hand:

```sh
claude mcp add whyfail -s user -- whyfail mcp
```

It is independent of the hook: the hook rewrites `Bash` and `WebFetch`, while the MCP
server gives Claude typed tools. Both can be installed together.

## Data handling

Only public Prow data is read. Do not pass private or embargoed logs to an external
service, and do not suggest an automatic `/retest` — `whyfail job` reports whether a
retest is reasonable.
