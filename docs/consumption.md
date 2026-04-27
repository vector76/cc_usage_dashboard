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
`*_used` summed over the period. When the `*_window_ends` timestamp
differs between two adjacent snapshots (i.e. the window reset between
them), the new window contributes only `curr.used`; the unobserved
tail of the prior window — between its last snapshot and the reset —
is treated as zero. This under-reports if the prior session kept
growing after the last snapshot, but in practice snapshots arrive
right up to window end, so the missed tail is small.

```
walk = [snapshot_at_or_before(period_start), ...snapshots in period]
total = 0
for prev, curr in pairs(walk):
    if same_window(prev.ends, curr.ends):    # within 10 min tolerance
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
- **Window-reset detection** treats two adjacent snapshots as belonging
  to the same window when their `*_window_ends` values agree within 10
  minutes. The userscript computes `window_ends` as
  `Date.now() + minutesUntilReset`, so two snapshots in the same window
  can drift by a few minutes between sends; actually-different windows
  are at least multiple hours apart, so the generous tolerance is safe.
  (The reanchor logic in `windows.reanchorIfStale` uses a tighter
  2-minute tolerance because it only absorbs minute-rounded jitter on
  the *same* reset boundary.)
- **Unknown-model events** are still counted in `events_without_cost`
  and excluded from `consumed_usd_equivalent`. They have no effect on
  the percent numbers.
