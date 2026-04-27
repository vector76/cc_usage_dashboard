# Data model

A single SQLite database file owned by the trayapp. WAL mode for crash-safety. Single
writer (the trayapp); the dashboard, CLI, and userscript all reach the DB only through
the HTTP API.

## Tables

### `usage_events`

One row per recorded Claude Code invocation (or per assistant message, depending on
source granularity).

| Column                | Type         | Notes                                          |
|-----------------------|--------------|------------------------------------------------|
| id                    | INTEGER PK   | Autoincrement.                                 |
| occurred_at           | TIMESTAMP    | Wall-clock time of the event.                  |
| source                | TEXT         | `tailer` \| `cli` \| `hook`. Provenance.       |
| session_id            | TEXT         | Claude Code session, if known.                 |
| message_id            | TEXT         | Assistant message ID, if known. Used for dedup.|
| project_path          | TEXT         | Decoded path the session ran in, if known.     |
| input_tokens          | INTEGER      | Required.                                      |
| output_tokens         | INTEGER      | Required.                                      |
| cache_creation_tokens | INTEGER      | Nullable.                                      |
| cache_read_tokens     | INTEGER      | Nullable.                                      |
| cost_usd_equivalent   | REAL         | Dollar-equivalent quota cost. Nullable if      |
|                       |              | neither the source nor the price-table         |
|                       |              | computation produced a value.                  |
| cost_source           | TEXT         | `reported` (came from JSONL/POST) \|           |
|                       |              | `computed` (derived from tokens × price table).|
| model                 | TEXT         | Model name when known. Needed for cost compute.|
| raw_json              | TEXT         | Original event payload for forensic replay.    |

Constraints:

- Unique `(session_id, message_id)` where both non-null. Lets the tailer and a hook-driven
  POST coexist without double-counting.
- Index on `occurred_at` for window queries.

### `quota_snapshots`

One row per authoritative read of the quota state from the dashboard (via userscript or,
later, headless scrape).

| Column                | Type         | Notes                                          |
|-----------------------|--------------|------------------------------------------------|
| id                    | INTEGER PK   |                                                |
| observed_at           | TIMESTAMP    | When the snapshot was read by the source.      |
| received_at           | TIMESTAMP    | When the trayapp received the POST.            |
| source                | TEXT         | `userscript` \| `headless` \| `manual`.        |
| session_used          | REAL         | "Current session" % used (0–100). Nullable.    |
| session_window_ends   | TIMESTAMP    | When the current 5-hour session resets.        |
| weekly_used           | REAL         | "All models" weekly % used (0–100). Nullable.  |
| weekly_window_ends    | TIMESTAMP    | When the weekly window resets.                 |
| raw_json              | TEXT         | Full payload for replay.                       |

### `windows`

Derived/cached state for the current and recent session and weekly windows. Maintained by
the trayapp from `usage_events` + `quota_snapshots`.

| Column          | Type      | Notes                                                |
|-----------------|-----------|------------------------------------------------------|
| id              | INTEGER PK|                                                      |
| kind            | TEXT      | `session` \| `weekly`. (`session` = the rolling      |
|                 |           | 5-hour "Current session" window in the Claude UI.)   |
| started_at      | TIMESTAMP | First-use timestamp (session) or week boundary.      |
| ends_at         | TIMESTAMP | Computed.                                            |
| baseline_total  | REAL      | "% used" anchor at the most-recent in-window         |
|                 |           | snapshot. Column name is legacy from the dollar-     |
|                 |           | denominated era and may be renamed; values are now   |
|                 |           | percentages (0–100).                                 |
| baseline_source | TEXT      | Snapshot ID or `default` if assumed.                 |
| closed          | INTEGER   | 0 while active, 1 once expired.                      |

This table exists so historical windows can be queried efficiently for charts without
recomputing from raw events every time.

### `slack_samples`

Optional: time-series of slack signal values, sampled when the slack endpoint is queried.
Useful for tuning the queue heuristics later. Can be turned off.

| Column         | Type       | Notes                                |
|----------------|------------|--------------------------------------|
| id             | INTEGER PK |                                      |
| sampled_at     | TIMESTAMP  |                                      |
| slack_fraction | REAL       | (percent_expected − percent_used)/100. |
| window_id      | INTEGER    | FK into `windows`.                   |

### `slack_releases`

One row per `POST /slack/release`. Lets us audit the queue's decisions against the
slack values we exposed at the time. Distinct from `slack_samples`: this is a discrete
event log, not a time-series.

| Column            | Type       | Notes                                            |
|-------------------|------------|--------------------------------------------------|
| id                | INTEGER PK |                                                  |
| released_at       | TIMESTAMP  | Reported by the queue (its clock).               |
| received_at       | TIMESTAMP  | When the trayapp received the POST. Skew check.  |
| job_tag           | TEXT       | Free-form identifier the queue chose.            |
| estimated_cost    | REAL       | Dollar-equivalent the queue expected the job to cost. |
| slack_at_release  | REAL       | The slack value the queue saw on its prior `/slack`. |
| window_id         | INTEGER    | FK into `windows`. Which window the queue sized against. |

### `parse_errors`

Anything the tailer or HTTP handlers couldn't make sense of. Helps detect schema drift
without losing data.

| Column     | Type      | Notes                       |
|------------|-----------|-----------------------------|
| id         | INTEGER PK|                             |
| occurred_at| TIMESTAMP |                             |
| source     | TEXT      |                             |
| reason     | TEXT      |                             |
| payload    | TEXT      | Raw input that failed.      |

## Units note

`usage_events.cost_usd_equivalent` is in dollars (raw or computed from token × price
table) — it's the per-event dollar input the consumption report sums.

`quota_snapshots.session_used` and `quota_snapshots.weekly_used` are **percentages**
(0–100) scraped from the `claude.ai/settings/usage` page, since that's the only
quota figure Anthropic actually exposes there ("6% used", "23% used"). The dashboard
no longer carries dollar-denominated quota totals, because there is no such number
in the source UI to anchor against.

## Cost source

`cost_usd_equivalent` is not always present in the JSONL — Claude Code computes it from
the model + token counts, and depending on version may or may not serialize it. The
trayapp therefore needs a small **price table** keyed by model name (input / output /
cache-read / cache-creation per million tokens), and computes cost on ingest if the raw
field is absent. Both the raw and computed values are stored; the dashboard prefers the
raw value when present and labels computed values explicitly so the user knows when
they're seeing our estimate vs. Anthropic's. The price table is config, refreshed by
hand when Anthropic updates rates.

## Retention

- `usage_events`: keep forever. Volume is small (KB/day at most).
- `quota_snapshots`: keep forever.
- `windows`: keep forever.
- `slack_samples`: optional, default off. If on, retain 90 days.
- `slack_releases`: keep forever. Volume is tiny (one row per released job).
- `parse_errors`: retain 30 days, with a count-only summary kept indefinitely.

## Migrations

Use a simple `schema_version` table and apply numbered migration files at startup. No
ORM. Plain SQL is fine at this scale.
