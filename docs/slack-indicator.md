# Slack indicator

The slack indicator answers: "Are we underconsuming the quota relative to a steady
burn-down? If so, by how much?" An external job queue uses this signal to decide whether
to release low-priority work that is only worth running for free.

## Definition

Let:

- `Q` = total quota for the current window (5-hour or weekly).
- `t0` = window start.
- `t1` = window end.
- `t` = now.
- `U(t)` = cumulative consumption since `t0`.

Define expected consumption as a uniform burn, clamped to the window:

```
progress(t)  = clamp((t - t0) / (t1 - t0), 0, 1)
E(t)         = Q * progress(t)
slack(t)     = E(t) - U(t)
slack_fraction(t) = slack(t) / Q
```

The clamp matters at the boundaries:

- **Before window starts** (`t < t0`): a 5-hour window only begins on first use. If no
  events have occurred yet, the window is undefined; the API returns
  `release_recommended=false` and a null `slack_fraction` rather than computing.
- **After window ends** (`t > t1`): `progress=1`, `E=Q`. Slack is just `Q - U`, but at
  this point any unused budget is already forfeit, so the value is informational only
  and `release_recommended=false`.

Positive slack ⇒ the user is below pace and has free capacity that will expire at `t1`.
Negative slack ⇒ the user is ahead of pace; releasing low-priority work would risk
exhausting the quota before the user's real work completes.

## Two windows, one signal

There are two simultaneously active windows: 5-hour and weekly. The slack indicator must
not release work that fits the 5-hour pace but blows the weekly budget, or vice versa.

The combined signal is:

```
slack_combined = min(slack_fraction_5h, slack_fraction_weekly)
```

A job is releasable only when both windows show slack. The `min` is conservative on
purpose: any window that is ahead of pace blocks the queue.

## Don't fight the user

The signal is not enough on its own. The slack endpoint applies three server-side gates;
the queue is expected to apply one client-side gate of its own.

### Server-side gates (set `release_recommended`)

1. **Headroom gate.** Release work only if `slack_fraction >= release_threshold`
   (suggested default: `0.10`). Note: this threshold is in *quota-fraction* units, not
   "% below pace." The relationship between the two is
   `slack_fraction = progress(t) * (1 - U/E)`, so the same threshold demands more
   underutilization early in a window than late. This is the desired behavior: early
   on we don't yet know the user's intent; late on, unspent budget is about to expire
   and is genuinely safe to consume.

2. **Priority quiet gate.** If the user has issued a Claude Code request in the last
   `priority_quiet_period` (suggested default: 5 minutes), refuse release. Prevents
   racing the user's interactive loop and stealing their headroom.

3. **Baseline freshness gate.** See dedicated section below.

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
  "five_hour": {
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
  "release_recommended": true,
  "gates": {
    "headroom":          true,
    "priority_quiet":    true,
    "baseline_freshness": true
  }
}
```

`release_recommended` is true iff every gate passes. The queue can also read the raw
fractions and apply its own logic.

## Baseline freshness gate

If the most recent snapshot is older than `baseline_max_age` (suggested 48 hours) **and**
passive consumption since then exceeds `baseline_drift_threshold` (suggested 25% of
quota), the freshness gate fails. Rationale: we have low confidence in `Q` and might be
over-releasing into a near-exhausted window.

## Why uniform burn for `E(t)`

Uniform is simple and roughly matches how a user spreads work across a day. Sophisticated
alternatives (forecast based on trailing N windows; learn the user's diurnal pattern) are
deferred. They don't change the architecture — only the body of `E(t)`.

## Slack consumption logging

When a queue caller releases a job, it should include the job tag in a `POST /slack/release`
call. The trayapp records the release in `slack_log` so the dashboard can show "free work
done this week" and tune thresholds based on actuals.

## Failure modes

| Condition                                    | Behavior                                |
|----------------------------------------------|-----------------------------------------|
| No baseline snapshot ever recorded           | `release_recommended=false`. Show alert.|
| Window has not started (no events yet)       | `slack_fraction = null`, `release_recommended=false`. |
| Negative slack on either window              | `release_recommended=false`.            |
| Baseline drift detected                      | Freshness gate fails.                   |
| Trayapp restart mid-window                   | Recovers from `windows` table state.    |
