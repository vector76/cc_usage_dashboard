# Dashboard page

The page at `/` (and `/dashboard`). It polls `/api/dashboard/state` and
`/consumption` every 10 seconds and is the surface a user leaves open on a
second monitor, so its layout is ordered by how often a glance needs each
thing rather than by how the data is produced.

Top to bottom:

1. **Title.**
2. **Burn-down** — the session and weekly charts and the model-family
   legend, with the slack-release flag on the heading line.
3. **Consumption** — a period picker and the link to `/report` on the
   heading line, then the measured spend for that period, the allowance
   estimates derived from it, and the snapshot freshness line.
4. **Feedback** — collapsed, badge-only until expanded.

## Why the charts come first

They are the thing being watched. Everything else on the page is either a
single word (the slack flag), a number that changes slowly (consumption
over 24h or more), or a panel that is collapsed by default. Putting a
status table above the charts cost the charts a screenful of scroll to
report values that mostly never change — "Paused: no", "Parse errors: 0" —
and the one field in it worth watching, the slack flag, is now a word on
the Burn-down heading line at no vertical cost at all.

## The slack-release flag

`slackReleaseState` (`static/summary.js`) reduces the state payload to
`yes` / `no` / `paused` / `unknown`, and the page maps that to a label and
colour. Only `yes` and `paused` are styled loudly.

`paused` outranks the gate verdict in both directions. Pausing stops the
observations the headroom gates evaluate, so whatever they last concluded
describes a stale world — including a stale `yes`. Showing `no` while
paused would read as a decision that slack is unavailable, when the truth
is that the question cannot be answered until collection resumes. The
information really is lost while paused; saying so is better than guessing.

## The period picker

Offers 24h / 7d / 30d, defaults to 24h, and remembers the choice in
`localStorage` under `cc-dashboard-consumption-period`. Changing it
re-fetches immediately rather than waiting out the remaining poll interval.

Only the three offered values are ever accepted back out of storage. The
value is interpolated into a URL query, so a stale or tampered storage
entry must not be able to steer the request; the server also refuses to
echo a rejected period back into the response body (see
`server.handleConsumption`).

The longer periods are not just a bigger sample — they are the ones that
make the allowance estimate below useful, because a 24h period frequently
leaves the weekly percentage under the estimate's floor.

## The link to `/report`

It sits at the far end of the Consumption heading row, opposite the period
picker. The range report is a per-model drill-down on exactly these
numbers, so it belongs beside them rather than above the charts, where it
was reserving a line of its own to advertise a page nobody visits on a
glance. The two controls are deliberately at opposite ends of the row: the
picker changes what is on this page, the link leaves it.

The dashboard is the only thing that links to `/report` — the report links
back, but nothing else advertises it — so `TestDashboardIndexHTML` asserts
the link is still there.

## Snapshot freshness

`Last allowance snapshot` is the only survivor of the old status table: an
absolute stamp plus an age, e.g. `Sep 18 10:45 AM (2 m 29 s ago)`. The
stamp says what you are looking at; the age says whether to trust it.

Both halves derive from the server's clock — the age is
`last_snapshot_age_seconds` and the stamp is the server's `now` minus that
age — so a skewed client clock shifts neither. `fmtAge` clamps a negative
age to zero, since a disagreement between those two server-supplied fields
is skew, not time travel.

The other former status fields were dropped rather than relocated:
`parse_errors_24h` is already counted in the feedback badge, `now` is the
clock in the corner of the screen, and the session countdown is the
rightmost third of the session chart.

## No `innerHTML`

Every server-supplied value on this page reaches the DOM through
`textContent`, via the `span` / `metric` / `clear` helpers. The page
renders model names, warning messages, parse reasons and an echoed period
string, none of which it controls; string-concatenated markup would make
any of them a reflected-XSS sink. See `docs/design-decisions.md`.

## What is testable, and where

The charts are drawn inline in `index.html` — they are SVG-building code
with no interesting arithmetic to pin. The rules that *are* worth pinning
live in two require-able modules that the page loads as plain scripts and
`userscript/test/` exercises directly:

- `static/grouping.js` — polyline splitting, family order and colours.
- `static/summary.js` — allowance extrapolation, the slack-release state
  machine, age formatting.

`internal/server/dashboard_test.go` asserts both are reachable as
standalone routes, so a module that the tests cover can't quietly stop
being the module the page runs.
