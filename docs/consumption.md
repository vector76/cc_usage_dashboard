# Consumption report

A flat report of usage over a chosen period: dollar-equivalent cost from
`usage_events`, plus snapshot-derived percent-of-quota consumption split
into the session and weekly windows.

The endpoint stays flat: it reports the dollar number and the percent
numbers and nothing derived from both. We still don't synthesize a
"discount" or "value ratio" — those depend on what the user is paying,
which the dashboard doesn't model. The one relation we *do* draw is the
allowance extrapolation below, and it lives on the client because it is a
presentation choice with a threshold in it, not a measurement.

## Inputs

- `cost_usd_equivalent` summed over `usage_events.occurred_at` in the
  period. Same column the dashboard's per-window consumed totals use.
- `quota_snapshots.session_used` / `weekly_used` (each 0–100, snapshotted
  by the userscript) walked across the period.

## Percent-consumed derivation

The session and weekly numbers are the per-snapshot increases in
`*_used` summed over the period. For each adjacent pair the walker
asks "is `curr` the start of a fresh window?" — when yes it adds
`curr.used` as a new window's contribution; when no it adds
`max(0, curr.used - prev.used)`. The unobserved tail of the prior
window — between its last snapshot and the reset — is treated as
zero. This under-reports if the prior window kept growing after the
last snapshot, but in practice snapshots arrive right up to window
end, so the missed tail is small.

"Start" detection is per kind, because the persisted
`continuous_with_prev` column is decided by the userscript from
session-only signals (cold start, >15-min wall-clock gap, session %
decrease, or `session_window_ends` jump > 1 hr — see
`docs/userscript.md`). Applying that flag verbatim to the weekly walk
would turn every session reset inside the period into a phantom
weekly reset and re-charge `weekly_used` into the total each time.

- **Session walk** — `curr.continuous_with_prev` is the primary signal,
  but a flag of `false` alone is not enough to declare a reset. The
  userscript marks any > 15-minute wall-clock gap as `false`, even
  when the session window did not actually reset (the user just
  stepped away); accepting that at face value adds `curr.session_used`
  as phantom consumption for every idle gap. The walker requires
  positive numeric evidence — `curr.session_used < prev.session_used`
  — to count a `false` flag as a start. NULL (a pre-migration row) is
  still treated as a start so legacy snapshots cannot be silently
  misclassified as continuations of an unrelated prior window.
- **Weekly walk** — `curr` is a start when `curr.weekly_used <
  prev.weekly_used`. The persisted flag is ignored: weekly resets
  always drop `weekly_used` from ~high to ~0, and no session-window
  signal carries information about the weekly window.

```
walk = [snapshot_at_or_before(period_start), ...snapshots in period]
total = 0
for prev, curr in pairs(walk):
    if is_start(kind, prev, curr):
        total += curr.used
    else:
        total += max(0, curr.used - prev.used)

is_start("session", prev, curr):
    if curr.continuous_with_prev is NULL: return true   # legacy default
    if curr.continuous_with_prev: return false           # explicit cont
    return curr.session_used < prev.session_used         # require evidence
is_start("weekly",  prev, curr) = curr.weekly_used < prev.weekly_used
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
- **Window-reset detection** is per kind. The session walk consults
  the persisted `continuous_with_prev` flag but cross-checks an
  explicit `false` against a numeric drop in `session_used` before
  declaring a reset, so the userscript's 15-min-gap continuity break
  doesn't masquerade as a fresh session whenever the user steps away.
  The weekly walk ignores the flag entirely and infers a reset from a
  strict decrease in `weekly_used`, because the flag carries
  session-window signals that do not apply to the weekly window. The
  previous `windowMatchTolerance` / `sameWindow` Δt heuristic in
  `internal/consumption/consumption.go` is gone. (The reanchor logic
  in `windows.reanchorIfStale` still uses a tighter 2-minute tolerance
  because it only absorbs minute-rounded jitter on the *same* reset
  boundary; that is unrelated to connectivity and unaffected by this
  change.)
- **Session reset without a numeric drop is missed.** The session
  walker requires `curr.session_used < prev.session_used` to count a
  `continuous_with_prev=false` row as a fresh session. If a reset
  happens and the new session has already grown above the prior
  observed percent by the time the next snapshot arrives, the walker
  treats the row as a continuation and the new session's contribution
  is folded into a (small or zero) delta. Requires heavy use
  immediately after reset and a snapshot interval long enough for the
  ramp-up — uncommon in practice, and accepted as the cost of not
  inflating every 15-minute idle gap by a full session's worth.
- **Unknown-model events** are still counted in `events_without_cost`
  and excluded from `consumed_usd_equivalent`. They have no effect on
  the percent numbers.

## Allowance extrapolation (client-side)

The dashboard prints, under the measured numbers, an estimate of what
100% of a session window and 100% of a weekly window are worth in dollars:

```
est_full_allowance = consumed_usd / (consumed_pct / 100)
```

Both inputs cover the same period, so the division is just "dollars per
percent, scaled to a full window". If $20 moved the session gauge 50%, a
whole session window is worth about $40.

`extrapolateAllowance` in `internal/dashboard/static/summary.js` is the
single source of truth for the rule;
`userscript/test/summary.test.js` covers it.

### Why a single division rather than a fit

Both inputs already aggregate the whole selected period. Picking a longer
period is what averages out a burst of unusually cheap or expensive
traffic — that is the job the period picker does. A curve fit over the
snapshot series would add machinery without adding information, and would
have to re-derive the same window-start detection the percent walk above
already does.

### The 10% floor

Below 10% consumed the estimate is reported as "insufficient data" rather
than a number. The percent gauges are scraped from Anthropic's UI at
whole- or near-whole-percent resolution, so a reading of 3% carries about
±0.5% of quantization error — a ±17% band on the result, and worse the
closer to zero it gets. At 10% the band is down to ±5%, which is honest
enough to print. In practice this is what a 24h period usually hits on the
weekly window; 7d or 30d clears it.

### Percentages above 100

A period longer than one window reports more than 100% consumed — a 30-day
period spans roughly 140 session windows. That is more evidence, not less,
and the same division holds: "1416% consumed for $2480" is a
better-measured $175 per session window than any single window's reading.

### Zero dollars is not zero allowance

Percent comes from the userscript's snapshots; dollars come from
`usage_events`, written by the tailer. A half-configured install can have
one running and not the other, which would make the division print a
confident $0.00 allowance. `consumed_usd <= 0` is therefore also
"insufficient data".
