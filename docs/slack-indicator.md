# Slack indicator

The slack indicator answers: "Are we underconsuming the quota relative to a steady
burn-down? If so, by how much?" An external job queue uses this signal to decide whether
to release low-priority work that is only worth running for free.

## Definition

Let:

- `Q` = total quota for the current window (session or weekly). Now derived from
  `windows.baseline_total`, which since the v0.2 schema rewrite stores a
  percentage anchor (0–100) rather than a dollar amount. Math below is unitless
  in `Q`'s own units, but the dashboard / queue should treat the figures as
  unanchored to dollars until cross-source reconciliation lands.
- `t0` = window start.
- `t1` = window end.
- `t` = now.
- `U(t)` = cumulative consumption since `t0`. Currently sourced from
  `usage_events.cost_usd_equivalent` (in dollars). The unit mismatch between
  `Q` and `U` is known follow-up debt — the previous formulation assumed both
  were dollars, but Anthropic stopped exposing a per-window dollar quota.

Define expected consumption as a uniform burn, clamped to the window:

```
progress(t)  = clamp((t - t0) / (t1 - t0), 0, 1)
E(t)         = Q * progress(t)
slack(t)     = E(t) - U(t)
slack_fraction(t) = slack(t) / Q
```

The clamp matters at the boundaries:

- **Before window starts** (`t < t0`): a session window only begins on first use. If no
  events have occurred yet, the window is undefined; the API returns
  `release_recommended=false` and a null `slack_fraction` rather than computing.
- **After window ends** (`t > t1`): `progress=1`, `E=Q`. Slack is just `Q - U`, but at
  this point any unused budget is already forfeit, so the value is informational only
  and `release_recommended=false`.

Positive slack ⇒ the user is below pace and has free capacity that will expire at `t1`.
Negative slack ⇒ the user is ahead of pace; releasing low-priority work would risk
exhausting the quota before the user's real work completes.

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

1. **Session headroom gate.** Release work only if
   `session.slack_fraction >= session_surplus_threshold` (suggested default: `0.50`).
2. **Weekly headroom gate.** Release work only if
   `weekly.slack_fraction >= weekly_surplus_threshold` (suggested default: `0.10`).

   Two independent thresholds, not a `min(session, weekly)` combined fraction, because
   the two windows have very different time horizons: a 5-hour session can recover
   from over-burn within hours, while a weekly over-burn lingers for days. A high
   bar on the session and a low bar on the weekly captures "leave the user enough
   short-term headroom to do real work, but don't sit on excess long-term capacity."
   Both thresholds are in *quota-fraction* units. The relationship to pace is
   `slack_fraction = progress(t) * (1 - U/E)`, so the same threshold demands more
   underutilization early in a window than late.

3. **Priority quiet gate.** If the user has issued a Claude Code request in the last
   `priority_quiet_period` (suggested default: 5 minutes), refuse release. Prevents
   racing the user's interactive loop and stealing their headroom.

4. **Baseline freshness gate.** See dedicated section below.

5. **Not-paused gate.** The user can pause the slack signal from the tray menu (see
   `docs/tray-app.md`). When paused, the endpoint still computes and returns the
   numeric fields so dashboards keep working, but `paused: true` and the gate fails.
   A queue distinguishing "paused" from "below threshold" can read `paused` directly.

### Client-side gate (queue's responsibility)

4. **Per-job budget cap.** Each pending job has an estimated cost. The queue should
   only release a job when `estimated_cost <= slack * safety_factor` (suggested `0.5`).
   The slack endpoint can't apply this gate — it doesn't know about specific jobs —
   but it exposes `slack` in absolute units so the queue can compute it. Prevents one
   big job from eating all available headroom.

## API

```
GET /slack
```

Response:

```json
{
  "now": "2026-04-25T17:32:14Z",
  "session": {
    "window_start": "2026-04-25T14:02:11Z",
    "window_end":   "2026-04-25T19:02:11Z",
    "quota_total":  100.0,
    "consumed":     32.5,
    "expected":     45.7,
    "slack":        13.2,
    "slack_fraction": 0.132
  },
  "weekly": {
    "window_start": "2026-04-21T00:00:00Z",
    "window_end":   "2026-04-28T00:00:00Z",
    "quota_total":  2000.0,
    "consumed":     611.4,
    "expected":     1142.9,
    "slack":        531.5,
    "slack_fraction": 0.266
  },
  "slack_combined_fraction": 0.132,
  "priority_quiet_for_seconds": 432,
  "paused": false,
  "release_recommended": true,
  "gates": {
    "session_headroom":   true,
    "weekly_headroom":    true,
    "priority_quiet":     true,
    "baseline_freshness": true,
    "not_paused":         true
  }
}
```

`release_recommended` is true iff every gate passes. The queue can also read the raw
fractions and apply its own logic.

## Baseline freshness gate

The gate passes iff a snapshot exists and is no older than `baseline_max_age`
(suggested 48 hours). Missing snapshot fails the gate.

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
| `GET /slack`: snapshot older than `baseline_max_age` | Freshness gate fails.                 |
| Either endpoint: trayapp restart mid-window        | Recovers from `windows` table state.    |
| `POST /slack/release`: no window of requested kind | HTTP 409. Queue should not have called `/release` if the corresponding `GET /slack` reported `release_recommended=false`; this catches the misuse. |
| `POST /slack/release`: required field missing      | HTTP 400.                               |
