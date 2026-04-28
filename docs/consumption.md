# Consumption report

A flat report of usage over a chosen period: dollar-equivalent cost from
`usage_events`, plus snapshot-derived percent-of-quota consumption split
into the session and weekly windows. The relation between the dollar
number and the percent numbers is left to the reader; we don't synthesize
a "discount" or "value ratio" — those depend on what the user is paying,
which the dashboard doesn't model.

## Inputs

- `cost_usd_equivalent` summed over `usage_events.occurred_at` in the
  period. Same column the dashboard's per-window consumed totals use.
- `quota_snapshots.session_used` / `weekly_used` (each 0–100, snapshotted
  by the userscript) walked across the period.

## Percent-consumed derivation

The session and weekly numbers are the per-snapshot increases in
`*_used` summed over the period. The walker reads the explicit
`continuous_with_prev` flag persisted on each snapshot row to decide
whether two adjacent snapshots belong to the same window: a `true`
flag means the later snapshot continues the prior window and
contributes the non-negative delta; a `false` (or NULL) flag means
the later snapshot is the start of a fresh segment, so the new
window contributes only `curr.used`. The unobserved tail of the prior
window — between its last snapshot and the reset — is treated as zero.
This under-reports if the prior session kept growing after the last
snapshot, but in practice snapshots arrive right up to window end, so
the missed tail is small.

NULL is treated as start (matching the bead-1 default), so snapshots
written before migration v5 cannot be silently misclassified as
continuations of an unrelated prior window.

```
walk = [snapshot_at_or_before(period_start), ...snapshots in period]
total = 0
for prev, curr in pairs(walk):
    if curr.continuous_with_prev:        # explicit true; NULL is start
        total += max(0, curr.used - prev.used)
    else:
        total += curr.used
```

A multi-window period can still exceed 100% — each fully-used session
contributes ~100% to the running total — but ordinary days won't.

If no snapshots exist for the kind in or before the period, the percent
field is `null` (couldn't measure), not `0`.

## API

```
GET /consumption?period=7d
```

```json
{
  "period": "7d",
  "period_start": "2026-04-19T00:00:00Z",
  "period_end":   "2026-04-26T00:00:00Z",
  "consumed_usd_equivalent": 312.40,
  "consumed_session_pct": 740.0,
  "consumed_weekly_pct": 95.0,
  "events_total": 1283,
  "events_with_reported_cost": 612,
  "events_with_computed_cost": 568,
  "events_without_cost": 103
}
```

`consumed_session_pct = 740` over a 7-day period is normal — that's
roughly 7 sessions/day × 7 days × ~15% per session, give or take. It is
not bounded at 100.

## Caveats

- **Snapshot density.** The percent numbers are only as accurate as the
  snapshot stream; an hour-long gap between snapshots gets attributed to
  whichever pair of snapshots brackets it. Periods with few snapshots
  under-report.
- **Window-reset detection** is now driven by the explicit
  `continuous_with_prev` flag stored on each snapshot row, not by a
  tolerance comparison on `*_window_ends`. The userscript decides the
  flag at the source (cold start, > 15 min wall-clock gap, session
  percent decrease, or `session_window_ends` jump > 1 hr → start;
  otherwise continuation — see `docs/userscript.md`); the consumption
  walker trusts the flag verbatim. The previous `windowMatchTolerance`
  / `sameWindow` Δt heuristic in `internal/consumption/consumption.go`
  is gone. (The reanchor logic in `windows.reanchorIfStale` still uses
  a tighter 2-minute tolerance because it only absorbs minute-rounded
  jitter on the *same* reset boundary; that is unrelated to
  connectivity and unaffected by this change.)
- **Unknown-model events** are still counted in `events_without_cost`
  and excluded from `consumed_usd_equivalent`. They have no effect on
  the percent numbers.
