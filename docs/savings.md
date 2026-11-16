# Savings

What `whyfail gain` measures, and what it only models.
Back to the [README](../README.md).

Every invocation is recorded locally in `~/.cache/whyfail/stats.jsonl`. The file is
rotated to `stats.jsonl.1` at 5 MB, and `gain` reads both, so totals span the last
two generations rather than resetting:

```sh
whyfail gain
whyfail gain --history
whyfail gain --by repo --since 7d
```

It reports bytes read versus emitted, what came from the cache versus the network,
which job verdicts you spent your calls on, how far you are from the next round
milestone, and — under `--fun` — the most expensive single investigation, what a
blind retest would have won, and the hour you triage most.

Measured numbers and modelled ones are kept apart: anything prefixed `~` is an
estimate, derived from `--bytes-per-token` (default 4) and `--baseline`.
`doctor` and `init` read no artifacts, so they record nothing and cannot inflate the
totals.

Disable recording with `--no-stats` or `WHYFAIL_NO_STATS=1`. `WHYFAIL_CACHE_DIR`
relocates both `stats.jsonl` and the object cache.

See [JSON output](json-output.md) for the `gain` document shape and
[CLI reference](cli-reference.md#flags) for the flag defaults.
