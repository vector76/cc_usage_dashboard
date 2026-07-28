# Slack indicator

The slack indicator answers: "Are we underconsuming the quota relative to a steady
burn-down? If so, by how much?" An external job queue uses this signal to decide whether
to release low-priority work that is only worth running for free.

## Definition

The signal is computed entirely in percent-of-quota units. Both inputs come
from the userscript's snapshots — usage_events / cost_usd_equivalent does
not enter the slack signal at all (the consumption report owns dollars).

Let:

- `t0` = current window start, `t1` = window end, `t` = now.
- `percent_used(t)` = the latest in-window snapshot's `session_used` (or
  `weekly_used`) value, kept current on `windows.baseline_percent_used` by
  the windows engine.

```
progress(t)        = clamp((t - t0) / (t1 - t0), 0, 1)
percent_expected   = 100 * progress(t)              # uniform pace to 100% by t1
slack_fraction     = (percent_expected - percent_used) / 100   # in [-1, +1]
```

Positive `slack_fraction` ⇒ user is below pace and has free capacity that
will expire at `t1`. Negative ⇒ user is ahead of pace; releasing
low-priority work would risk exhausting the quota before the user's real
work completes.

Boundaries:

- **Before window starts** (`t < t0`): session windows only begin on first
  use; weekly is anchored from snapshots. If no in-window snapshot has
  arrived, `percent_used` and `slack_fraction` are null and the headroom
  gate fails (no measurement = don't release).
- **After window ends** (`t > t1`): `progress=1`, `percent_expected=100`.
  Slack is informational only; `release_recommended=false`.

Threshold meaning: `slack_fraction ≥ T` means "the unused fraction of the
full quota currently held in surplus relative to uniform pace is at least
T." A `0.50` session surplus only becomes achievable past 50% elapsed.

## Two windows, one signal

There are two simultaneously active windows: session (5-hour) and weekly. The slack
indicator must not release work that fits the session pace but blows the weekly
budget, or vice versa.

The combined signal is:

```
slack_combined = min(slack_fraction_session, slack_fraction_weekly)
```

A job is releasable only when both windows show slack. The `min` is conservative on
purpose: any window that is ahead of pace blocks the queue.

## Don't fight the user

The signal is not enough on its own. The slack endpoint applies three server-side gates;
the queue is expected to apply one client-side gate of its own.

### Server-side gates (set `release_recommended`)

1. **Session headroom gate.** Release work if ANY of:
   - `session.slack_fraction >= session_surplus_threshold` (default `0.50`), or
   - `session.percent_used <= 100 * (1 - session_absolute_threshold)` (default
     `0.98` → percent_used ≤ 2), or
   - the session window is absent entirely (no active 5-hour session row, i.e.
     the user is between sessions). This deadlock-breaker disjunct lets slack
     fire during inactive limbo when `session_active=false` has closed the
     window early — without it the gate would stay closed forever in limbo,
     since pace and percent_used are undefined when there's no window to
     measure against.
2. **Weekly headroom gate.** Release work if ANY of:
   - `weekly.slack_fraction >= weekly_surplus_threshold` (default `0.10`), or
   - `weekly.percent_used <= 100 * (1 - weekly_absolute_threshold)` (default
     `0.80` → percent_used ≤ 20), or
   - the weekly window is absent entirely (no active 7-day weekly row).
     Symmetric to the session deadlock-breaker: the windows engine refuses
     to mint a phantom weekly under limbo (see `docs/no-active-session.md`),
     so without this branch the gate would deadlock at "no weekly window to
     gate against" — exactly when the user has the most quota free.

   Two independent thresholds (instead of a `min(session, weekly)` combined
   fraction) because the two windows have very different time horizons: a
   5-hour session can recover from over-burn within hours, while a weekly
   over-burn lingers for days. A high bar on the session and a low bar on
   the weekly captures "leave the user enough short-term headroom to do
   real work, but don't sit on excess long-term capacity." `slack_fraction`
   is in fraction-of-quota units (range [-1, +1]).

   The absolute-threshold leg lets weekly slack activate early in the week
   before pace-relative surplus has accrued. Without it the gate would
   stay closed for the first day or two of every week even with no usage,
   defeating the purpose of harvesting unused capacity.

3. **Fable headroom gate.** The same two-leg rule as the weekly gate, applied
   to the "Fable" weekly sub-row and reusing the *same* thresholds — the two
   rows share a window, so a separate pace horizon would be meaningless.
   Release work if ANY of:
   - `fable_weekly.slack_fraction >= weekly_surplus_threshold`, or
   - `fable_weekly.percent_used <= 100 * (1 - weekly_absolute_threshold)`, or
   - **the sub-row is not currently being reported.**

   The gate exists because "All models" is an aggregate: it can sit deep in
   the green while the Fable sub-quota is nearly exhausted, and releasing
   free work then spends exactly the capacity the user is about to need. When
   both rows are reported, both must clear the same percentage limit and
   either one over it withdraws slack; when only the aggregate is reported,
   only the aggregate has to clear it.

   The third disjunct **fails open**, unlike the session/weekly deadlock-
   breakers which fail open for a different reason. Here it is a
   compatibility stance: `fable_weekly_used` has no backfill (see
   `docs/data-model.md`), so "absent" means "not observed" — pre-July-2026
   history, userscripts predating the extractor change, accounts whose page
   does not render the sub-row, and the day the page stops rendering it
   altogether must all behave exactly as they did before this gate existed,
   rather than deadlocking the queue on data that will not arrive.

   Which reading counts as "currently reported" is the subtle part — see
   dedicated section below.

4. **Baseline freshness gate.** See dedicated section below.

5. **Not-paused gate.** The user can pause the slack signal from the tray menu (see
   `docs/tray-app.md`). When paused, the endpoint still computes and returns the
   numeric fields so dashboards keep working, but `paused: true` and the gate fails.
   A queue distinguishing "paused" from "below threshold" can read `paused` directly.

### Client-side gate (queue's responsibility)

6. **Per-job budget cap.** Each pending job has an estimated cost in dollars or
   percent-of-quota — that's the queue's choice. The slack endpoint exposes only
   the unitless `slack_fraction`, so the queue must convert its job estimate into
   the same percent-of-quota units before applying any "fits in slack" check.
   Prevents one big job from eating all available headroom.

## Which Fable reading the gate uses

There is no `windows` column to read — the sub-row is deliberately not its own
window kind — so the reading comes from `quota_snapshots` directly, under two
filters.

**The newest row that parsed the weekly section wins, and its fable column is
taken at face value.** The userscript reads every bar in a single DOM scan and
omits what it did not find (see `docs/userscript.md`), so a row carrying
`weekly_used` but no `fable_weekly_used` is a positive statement that the
sub-row was absent at that instant — and it supersedes every earlier reading.
Selecting the newest *non-null* fable value instead would let the last real
reading gate for the remainder of the week after the row disappeared, which is
exactly the case the fail-open stance exists to cover.

Rows that never parsed the weekly section (`weekly_used` null — session bar
only) are skipped: they say nothing either way about the sub-row, so letting one
answer the question would open the gate on no evidence.

**The window bounds are the second filter.** The sub-row resets with the weekly
window, so a pre-window reading describes last week's quota and would otherwise
pin the gate shut for the whole of the new week.

A sub-row that is present but transiently unparsed — the weekly section
rendering before the sub-row hydrates — therefore reads as absent for one
observation. That fails open, the intended direction, and self-corrects on the
next post rather than persisting.

## API

```
GET /slack
```

Response:

```json
{
  "now": "2026-04-25T17:32:14Z",
  "session": {
    "window_start":     "2026-04-25T14:02:11Z",
    "window_end":       "2026-04-25T19:02:11Z",
    "percent_used":     32.5,
    "percent_expected": 45.7,
    "slack_fraction":   0.132
  },
  "weekly": {
    "window_start":     "2026-04-21T00:00:00Z",
    "window_end":       "2026-04-28T00:00:00Z",
    "percent_used":     30.6,
    "percent_expected": 57.1,
    "slack_fraction":   0.266
  },
  "fable_weekly": {
    "window_start":     "2026-04-21T00:00:00Z",
    "window_end":       "2026-04-28T00:00:00Z",
    "percent_used":     41.0,
    "percent_expected": 57.1,
    "slack_fraction":   0.161
  },
  "slack_combined_fraction": 0.132,
  "paused": false,
  "release_recommended": true,
  "gates": {
    "session_headroom":   true,
    "weekly_headroom":    true,
    "fable_headroom":     true,
    "baseline_freshness": true,
    "not_paused":         true
  }
}
```

`release_recommended` is true iff every gate passes. The queue can also read
the raw fractions and apply its own logic. `percent_used` and `slack_fraction`
are null whenever no in-window snapshot has arrived; in that state the
corresponding headroom gate fails.

`fable_weekly` borrows the weekly window's bounds and pace, differing only in
`percent_used`; it is `null` whenever the sub-row is not currently being
reported (see "Which Fable reading the gate uses"), and unlike the other two
blocks a `null` there passes its gate rather than failing it.
`slack_combined_fraction` stays
`min(session, weekly)` and deliberately excludes fable — existing consumers
read it as the two-window pace signal, and the fable constraint is expressed
through the `fable_headroom` gate instead.

## Baseline freshness gate

The gate passes iff a snapshot exists and is no older than `baseline_max_age`
(default 8 minutes). Missing snapshot fails the gate.

The freshness clock reads the most-recent `quota_snapshots.received_at`,
which the server's write-time slide (see `docs/data-model.md`) refreshes
on every identical continuation as well as on net-new rows. Combined with
the userscript's 60-second backstop and freshness-driven dedup (see
`docs/userscript.md`), an active page that ticks its "Resets in …" text
each minute keeps `received_at` well within the 8-minute window. A page
that produces no meaningful change for longer than `baseline_max_age` —
for example a deeply frozen limbo state with no fresh polls landing — is
indistinguishable from "userscript stopped" by this gate alone, which is
the conservative behaviour: the gate fails and `release_recommended`
goes false.

This is the only thing keeping `release_recommended` honest when the
userscript stops posting (page closed, tampermonkey down, browser killed).
Without it, queued work would keep draining quota against a frozen
`percent_used` snapshot. The threshold is deliberately tight because
"believe the snapshot" is the entire premise of the slack signal.

## Why uniform burn for `E(t)`

Uniform is simple and roughly matches how a user spreads work across a day. Sophisticated
alternatives (forecast based on trailing N windows; learn the user's diurnal pattern) are
deferred. They don't change the architecture — only the body of `E(t)`.

## Slack consumption logging

When a queue caller releases a job, it should `POST /slack/release` to record the
decision. The trayapp writes a row to `slack_releases` so the dashboard can show
"free work done this week" and so we can tune thresholds against actuals.

Request body:

```json
{
  "released_at": "2026-04-26T12:34:56Z",
  "job_tag":          "nightly-lint",
  "estimated_cost":   1.20,
  "slack_at_release": 8.40,
  "window_kind":      "session"
}
```

- `released_at`: when the queue made the decision (the trayapp also records its own
  `received_at`, so clock skew is observable).
- `job_tag`: free-form identifier for the job; goes to `slack_releases.job_tag`.
- `estimated_cost`: the queue's estimate in dollar-equivalent units. Lets us audit
  the per-job budget cap retrospectively.
- `slack_at_release`: the absolute slack value the queue saw on the preceding `GET
  /slack`. Detects races where slack changed between the query and the release.
- `window_kind`: which window the queue was sizing against (`session` | `weekly`).
  Optional; defaults to `session` since that's the binding window most of the time.
  The trayapp resolves this to a `windows` row at insert time, picking the window of
  the requested `kind` that contains `released_at`. If the window rolled over between
  `GET /slack` and `POST /slack/release` (rare but possible), the FK will point at
  the *new* window, while `slack_at_release` still reflects the *old* window's slack;
  this is an audit signal, not a bug — the row will show an unusually high
  `slack_at_release` relative to the new window's quota and is easy to filter.

A follow-up `POST /slack/release/<id>/complete` with `actual_cost` is deferred to v2;
v1 just records the release decision.

## Failure modes

| Condition                                          | Behavior                                |
|----------------------------------------------------|-----------------------------------------|
| `GET /slack`: no baseline snapshot ever recorded   | `release_recommended=false`. Show alert.|
| `GET /slack`: window has not started (no events)   | `slack_fraction = null`, `release_recommended=false`. |
| `GET /slack`: negative slack on either window      | `release_recommended=false`.            |
| `GET /slack`: Fable sub-row ahead of pace and over the absolute floor | `fable_headroom` fails; `release_recommended=false` even if the aggregate weekly row is green. |
| `GET /slack`: account never reports the Fable sub-row | `fable_weekly = null`, `fable_headroom` passes (fails open). |
| `GET /slack`: Fable sub-row stops being reported mid-week | The first weekly-bearing snapshot without it supersedes the last reading: `fable_weekly = null`, gate opens and stays open. |
| `GET /slack`: newest snapshots parsed only the session row | Fable reading from the last weekly-bearing snapshot still stands; a session-only row cannot open the gate. |
| `GET /slack`: snapshot older than `baseline_max_age` | Freshness gate fails.                 |
| Either endpoint: trayapp restart mid-window        | Recovers from `windows` table state.    |
| `POST /slack/release`: no window of requested kind | HTTP 409. Queue should not have called `/release` if the corresponding `GET /slack` reported `release_recommended=false`; this catches the misuse. |
| `POST /slack/release`: required field missing      | HTTP 400.                               |
