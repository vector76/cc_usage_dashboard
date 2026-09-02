# Usage report (per-model, arbitrary range)

A flat report answering "what did the range from A to B cost, and which
models spent it": per-model token counts and dollars over any time range
the user picks. Served as its own page at `/report`, backed by
`GET /api/usage/breakdown`.

This is a sibling of `docs/consumption.md`, not a replacement. The
consumption report answers "how much of my quota did the last N hours
burn", mixing dollars with snapshot-derived percentages; this one answers
"where did the dollars go", and touches `quota_snapshots` not at all.

## Why a separate endpoint

`/consumption` is defined as a duration ending at *now*, and it carries the
session/weekly percent walks. Neither fits an arbitrary historical range:
the percent walk over "the third week of last month" is a different
question with different caveats, and grafting `start`/`end` onto the
existing response would have made a single payload mean two things
depending on which parameters the caller passed. `/api/usage/breakdown`
is the range-shaped endpoint; `/consumption` is unchanged.

## Range semantics

Parameters, in precedence order (see
`consumption.Calculator.ParseRange`):

- `start` + `end`, both RFC3339. Any offset is accepted and normalized to
  UTC.
- `start` alone — runs to now.
- `period` — `24h`, `7d`, `30d`; resolves to `[now-period, now)`.
- nothing — the last 24h.

An explicit `start` wins over `period`, so a client that leaves a stale
preset in the query string still gets the range its date inputs describe.
A bare `end` with neither a `start` nor a `period` is rejected rather than
given an invented start.

**The range is half-open, `[start, end)`.** This differs from
`/consumption`, which uses an inclusive upper bound. Half-open is required
here because these ranges are meant to be composed: adjacent ranges must
partition the data so "this week" plus "last week" sums to the two weeks
with the boundary event counted exactly once. Verified against the live DB
— eight consecutive daily ranges sum to a single eight-day range to the
cent and to the event.

A malformed or inverted range answers **400**, distinct from the 500 a
query failure produces. Neither body echoes the submitted parameters; the
value is logged instead, matching `handleConsumption`.

### Timezone

`occurred_at` is stored in UTC, and the endpoint only accepts absolute
instants. The local-time reasoning lives entirely in the client: the
report page reads `datetime-local` inputs (which carry no zone), converts
them through the `Date` constructor — which interprets them in the
browser's zone — and sends `toISOString()`. Presets like "Today" and
"This week" are computed from local midnight the same way.

This split is deliberate. A user asking for "July 20" means their July 20,
and a server that received a zoneless `2026-07-20T00:00` would have to
guess. Pushing the conversion to the only party that knows the zone means
the server never guesses, and the report page labels its output in local
time so the answer reads in the same units as the question.

## Measured vs estimated dollars

The response reports three cost totals, and the split is the point:

- `measured_cost_usd` — events whose `cost_source` is `reported` (the
  transcript told us) or `computed` (from `prices.yaml`).
- `estimated_cost_usd` — events priced at the price table's **ceiling**
  because their model is not in the table. `ingest.ResolveCost` prices an
  unrecognized model at the per-rate maximum across the whole table
  rather than leaving it NULL, so a newly released model can't silently
  vanish from every dollar total. Those dollars are a deliberate upper
  bound.
- `total_cost_usd` — their sum, for convenience.

Folding all three into one headline number would present a guess as a
measurement, which is the same reason `consumption.Result` keeps
`EventsWithCeilingCost` apart and the burn-down bars force ceiling-priced
events into the gray "other" family regardless of what their name
suggests. The report carries this through to the row level: a model row's
`estimated` flag is true when any part of its cost came from the ceiling,
the UI marks it "est." in gray, and a footnote explains it.

Per-model `cost_source` collapses the provenances that actually
contributed dollars: the lone source when they agree, `mixed` when they
don't, `none` when the row has no costed events. An absent cost is not a
provenance — a model with one computed event and three uncosted ones reads
`computed`, not `mixed`.

**Token counts carry no such caveat.** They are recorded per event
whether or not a price was available, so the four token columns are exact
even on a row whose dollars are an upper bound. A NULL cost is reported as
`events_without_cost` (per model and overall) rather than contributing
`$0`, so "we don't know" never reads as "it was free".

## Model families

`consumption.ModelFamily` is the single source of truth for the
opus/sonnet/fable/mythos/haiku/other classification. Mythos is its own
family so the report labels the model honestly, but the client colours it
the same red as fable: it is the separately gated build of the same model
and an account uses one or the other, never both on one chart. The dashboard's stacked
volume bars delegate to it (`dashboard.modelFamily`) and the report's
per-model swatches colour from it, so a model cannot be coloured one way
on the burn-down chart and another way in the report.

Client-side, `VOLUME_FAMILY_ORDER` / `VOLUME_FAMILY_COLORS` live in
`static/grouping.js` — shared by both pages rather than duplicated per
page. `userscript/test/families.test.js` enforces three things that would
otherwise drift silently: every ordered family has a colour, the JS family
set matches what the Go classifier can return, and no page redeclares the
constants locally.

## Why its own page

The dashboard polls `/api/dashboard/state` every 10 seconds. A 30-day
per-model scan on that timer would cost far more than it tells anyone, and
the report is something a user consults rather than watches. `/report`
fetches on demand — once on arrival, then on each preset or Run click —
and nothing on it runs on a timer.
