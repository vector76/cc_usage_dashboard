# No-active-session model

Anthropic's quota UI has a state where no quota window is currently
open: the corresponding row replaces its "Resets in N hr M min" /
"Resets <Day> H:MM PM" hint with the phrase **"Starts when a message
is sent."** This applies independently to the 5-hour "Current session"
row and the weekly "All models" row. This page documents how that
state propagates end-to-end through the trayapp, from userscript
detection to the slack gate, so behaviour in limbo is consistent and
intentional rather than an accident of which component happens to
default to what.

## Why this needs special handling

Without explicit handling, "no active session" produces wrong answers
at every layer:

- The window engine could mint a phantom 5-hour window from a snapshot
  alone, even though the user has not actually started a session.
- Once such a phantom exists, dashboard charts would render a real
  session window and users would think their quota was burning down.
- The slack endpoint's session headroom gate measures `percent_used`
  against `session_absolute_threshold`; if no real window exists,
  `percent_used` is undefined and the gate would stay closed forever,
  deadlocking the queue exactly when the user has the most slack
  available.

The fix is to treat "no active session" as a first-class state with a
tri-valued signal carried from the source through to the gate. No
component invents the state on its own; each one acts on the same
authoritative bit.

## Tri-state signal: `session_active` / `weekly_active`

Throughout the system, `session_active` and `weekly_active` are each
**tri-valued**:

- `true` — the corresponding window is genuinely active.
- `false` — limbo is positively detected. There is no active window
  and the source is sure of it.
- `unknown` (NULL / field absent) — the source cannot positively
  decide. Downstream consumers must not infer either presence or
  absence.

`true` is rarely emitted. The userscript never sets it; it relies on
the absence of the field to mean "I saw a normal Resets hint, so
nothing limbo-specific to report." Other ingestion paths may set it
explicitly when they have a positive truth signal.

The weekly path mirrors the session path one-to-one: same DOM scan,
same nullable INTEGER column (`weekly_active`, migration v6), same
"refuse to mint phantoms" guard in the windows engine, same
hypothetical placeholder in the dashboard. The only behavioural
asymmetry is that the weekly engine does **not** early-close an
existing real weekly row when limbo is observed (no analogue of the
session early-closure path); a stale phantom weekly row heals via
re-anchor when a snapshot supplies a real `weekly_window_ends`, or
via natural expiry.

## End-to-end flow

### 1. Userscript: detect limbo, emit `session_active=false` / `weekly_active=false`

`userscript/claude-usage-snapshot.user.js` scans both the Current
session row's DOM and the Weekly row's DOM for the literal text
"Starts when a message is sent" (case-insensitive) via a shared
`isLimboLabel(bar)` helper. When found on the session row, the
snapshot POST body includes `"session_active": false`; when found on
the weekly row, it includes `"weekly_active": false`. When not found,
the corresponding field is omitted — absence encodes "unknown," not
"active." Both fields are independent: the page commonly shows one
row in limbo while the other is normal. See `docs/userscript.md` and
`docs/data-sources.md`.

**Cadence under limbo is sparse and freshness-driven, not periodic.**
The userscript's freshness-driven dedup gate (see `docs/userscript.md`)
emits only on a meaningful-change signal. Inside limbo the visible
percent is frozen and the row text is constant ("Starts when a
message is sent"), so the only signals that fire are the limbo
text appearing or disappearing and — once the userscript is in limbo
— a strict *decrease* in the parsed "Last updated" age, which
indicates that claude.ai's own poll fetched a fresh page. Consequently
any consumer that previously assumed a snapshot every backstop tick
must instead treat the limbo observation stream as event-driven:
an event happens when limbo is entered, an event happens each time
a fresh poll lands, and an event happens when limbo is exited.
There is no fixed interval.

What reaches the DB is sparser still: the limbo-entered observation
inserts a new row, but each subsequent fresh-poll observation
typically matches every "match" field of the previous one, so the
server-side write-time slide (`docs/data-model.md`) collapses them
onto the existing row by refreshing its `observed_at`/`received_at`.
A limbo run therefore tends to surface as a single row whose
timestamps creep forward as fresh polls land, bracketed by the
limbo-entered start and a continuity-flagged-false row when limbo
exits.

### 2. Snapshot ingestion: persist tri-state to `quota_snapshots`

The `/snapshot` handler accepts `session_active` and `weekly_active`
as nullable booleans. The `quota_snapshots.session_active` column
(added by migration v4 `add_quota_snapshots_session_active`) and
`quota_snapshots.weekly_active` column (added by migration v6
`add_quota_snapshots_weekly_active`) are both nullable `INTEGER`
storing `0` for false, `1` for true, and `NULL` for unknown. The
pointer-typed DTO fields preserve the absent / explicit-false
distinction across the JSON boundary. See `docs/data-model.md`.

### 3. Window engine: refuse phantoms, close early, anchor on events

The windows engine treats `session_active=false` / `weekly_active=false`
as authoritative for the moment of observation.

For session windows, three behaviours flow from that:

- **Refuse to mint phantoms.** When ensuring a session window from a
  snapshot, the engine consults the most recent `session_active`
  value. If it is `false`, no window is created. Zero session windows
  is a permitted state.
- **Early closure of the active window.** When an active session
  window already exists and a snapshot reports `session_active=false`
  (with `session_used=0` as a defensive contradiction check), the
  engine closes the window at the snapshot's `observed_at` rather
  than waiting for the calendar 5-hour expiry. This produces a clean
  boundary between "real session" and "post-closure limbo" so later
  evidence can be attributed correctly.
- **Event-anchored open (fallback).** When no session window is
  active, `session_active` is not `false`, and the most recent
  snapshot lacks a future `session_window_ends`, the engine falls
  back to a `usage_events`-based opening: a window opens iff an
  event exists whose `occurred_at` postdates the most recent closed
  session window's `ends_at` (or no closed session window exists at
  all). The preferred path is still the snapshot's authoritative
  reset boundary when one is available; event-anchored opening
  exists so a tailer-only or hook-only host (no recent snapshot) can
  still produce a window when real activity is observed, instead of
  guessing a 5-hour boundary out of thin air.

For weekly windows, only the **refuse-to-mint** behaviour applies,
and it is gated more narrowly than the session version: when no real
open weekly row exists, no snapshot supplies a `weekly_window_ends`
that is **strictly in the future** (`findWeeklyBoundary()` returns
zero, or returns a timestamp ≤ `now`), AND the most recent snapshot
reports `weekly_active=false`, the engine returns without inserting a
row. The `After(now)` clause is critical: at every weekly reset
`findWeeklyBoundary()` returns the just-passed boundary from
pre-rollover snapshots, and a stale boundary is functionally the
same as no boundary — minting with it would produce a born-expired
row that the next tick closes and re-mints, a loop that floods the
table with zombie rows (38 closed rows accumulated in a single
~38-minute limbo gap before this guard was added). When a snapshot
supplies a *future* boundary, that boundary wins over the limbo
signal — this differs from the session path, where
`session_active=false` is checked before the boundary is consulted.
There is no early-closure path for weekly: an existing real weekly
row is left alone and heals via re-anchor (when a snapshot supplies
a real future boundary) or natural expiry.

`reanchorIfStale` carries the same future-only guard for the
existing-row path: it refuses to push an active window's `ends_at`
onto a past snapshot boundary, which would otherwise produce the
same born-expired loop one layer deeper.

The combined effect is that the windows table reflects what actually
happened, not what a snapshot's `session_used` / `weekly_used` value
alone would imply.

### 4. Dashboard: hypothetical rendering and Status panel

When the dashboard's `/api/dashboard/state` finds no active session
window, the backend synthesizes a **hypothetical** session window
spanning `[now, now + 5h]`; the frontend renders it ghosted: lighter
fill and stroke, italic font, with a two-line annotation reading "No
active session / projection if started now." The same treatment
applies to the weekly chart: when no active weekly window exists, the
backend synthesizes a hypothetical weekly window spanning
`[now, now + 7d]` and the frontend annotates it "No active weekly
window / projection if started now." The chart still has a curve to
look at, but the styling makes it unmistakable that this is a
projection rather than a measurement.

The Status panel reflects the same fact in text form: "not active"
when `state.session_active` is false, "active (Xh Ym remaining)"
otherwise. `state.session_active` is derived from the windows table
(a non-hypothetical real session row exists), not directly from the
raw snapshot column. The snapshot column reaches the panel only
indirectly: it triggers the windows engine to close or refuse the
real row, which then flips `state.session_active`. The panel and the
chart therefore always agree on whether a real session is open.

### 5. Slack gate: no-window short-circuit

The session headroom gate in `GET /slack` is a disjunction of three
legs:

1. Pace-relative surplus: `slack_fraction >= session_surplus_threshold`.
2. Absolute floor: `percent_used <= 100 * (1 - session_absolute_threshold)`.
3. **No active window at all** — the deadlock-breaker.

The third leg fires when the response payload's `session` block is
nil, which happens precisely when the windows engine has no active
session window (because limbo closed it early or because event-anchored
opening has not yet been triggered). Without leg 3, legs 1 and 2 would
both evaluate against undefined values and the gate would never pass —
exactly when the user has the most free capacity to spare. With leg 3,
the queue is unblocked the moment limbo is observed.

`session_absolute_threshold` parameterises leg 2 only; setting it to
`1.0` disables that leg without affecting the no-window short-circuit.
See `docs/configuration.md` for the threshold's semantics and
`docs/slack-indicator.md` for the full gate structure.

The weekly headroom gate has a parallel three-leg structure with the
same deadlock-breaker. When `ensureWeeklyWindow` refuses to mint
under `weekly_active=false`, `resp.Weekly` is nil, and the third leg
fires so the queue is unblocked — symmetric to the session path.
Before this change weekly-absent was unreachable in practice (the
engine always minted a calendar-fallback row), so the slack gate's
deadlock-breaker was session-only; the symmetric weekly leg was
added when the refuse-to-mint guard landed.

## Invariants

- The userscript never emits `session_active=true` or
  `weekly_active=true`. Absence of either field is the only way "I saw
  a normal Resets hint" is communicated.
- The trayapp never invents `session_active` / `weekly_active` from
  `*_used`. A snapshot with `weekly_used=0` and no `weekly_active`
  field is "unknown," not "limbo."
- A hypothetical window in the dashboard is never persisted to
  `windows`. It is a backend-synthesized rendering construct only.
- While the most recent snapshot has `session_active=false`, no new
  session window opens, regardless of arriving `usage_events` or
  later snapshots that still assert limbo. Recovery requires a newer
  snapshot whose `session_active` is not `false` (typically the
  userscript seeing a normal "Resets in …" hint again); from there
  either a future `session_window_ends` or a fresh `usage_event`
  past the last closed window can mint a new row.
- The weekly equivalent is narrower: `weekly_active=false` only blocks
  minting when no snapshot supplies a `weekly_window_ends` that is
  strictly in the future. A stale (past) boundary is treated the same
  as no boundary, both for the open path (`ensureWeeklyWindow`) and
  the re-anchor path (`reanchorIfStale`). Without the future-only
  guard, the engine would loop on close-and-mint of born-expired
  windows at every weekly reset.
