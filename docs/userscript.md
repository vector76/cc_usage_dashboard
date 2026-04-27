# Userscript

A Tampermonkey/Violentmonkey userscript that reads quota numbers from the `claude.ai`
dashboard and POSTs them to the local trayapp as authoritative snapshots.

## Why a userscript and not an extension

- **Zero install friction.** One `.user.js` file, drag into Tampermonkey, done.
- **Zero distribution overhead.** No Chrome Web Store review, no signing.
- **Easy to inspect.** Plain JS the user can read and modify.

A full extension would add background-script lifecycle, manifest v3 hassles, and storage
permissions — none needed for this use case.

## Behavior

The script is `@match`-injected on every `claude.ai/*` page but no-ops unless the URL
path is exactly `/settings/usage`. On that path:

1. Wait for at least one `[role="progressbar"][aria-label="Usage"]` node to render
   (MutationObserver, with a sane timeout).
2. **Anchor on `<h2>` section headings, not row labels.** For each progressbar, find
   the most recent preceding `<h2>` in document order. Only two sections are kept:
   - `Plan usage limits` → first bar in this section is "Current session"
     (% of the rolling 5-hour window).
   - `Weekly limits` → first bar in this section is the aggregate "All models"
     (% of the weekly limit).
   Sub-rows under "Weekly limits" (Sonnet only, Claude Design, future additions),
   the "Additional features" section (routines), and the extra-usage section are
   all ignored. Anchoring on section titles is more durable than matching row
   labels — Anthropic edits row text often, section headings rarely.
3. Read `aria-valuenow` (0–100) directly. We do not text-scrape the "X% used" label.
4. Parse the page's "Last updated: N minutes ago" indicator into a staleness
   delta. The percent values and the "Resets in …" hint are accurate as of
   that timestamp, not as of `Date.now()`. `observed_at` is back-dated by the
   delta; the session-reset timestamp uses the back-dated time as its base
   (`baseMs + Δ` rather than `now + Δ`). When the indicator can't be found
   the snapshot is treated as current.
5. Parse the row's reset hint into a UTC ISO timestamp:
   - "Resets in 3 hr 33 min" / "Resets in 19 min" → `observedAt + Δ`.
   - "Resets Thu 11:00 PM" → next future occurrence of that weekday at that
     local time, converted to UTC. Absolute clock-time hints are unaffected
     by page staleness.
   These land in `session_window_ends` / `weekly_window_ends` so the server can
   anchor the windows on Anthropic's actual reset boundary instead of a calendar
   guess.
6. POST to `http://localhost:27812/snapshot`.

**Trigger sources, in priority order:**

- **Persistent MutationObserver** on `document.body`, filtered to `aria-valuenow`
  attribute changes. Fires within milliseconds of claude.ai's own poll updating
  the DOM, even on backgrounded tabs.
- **5-min `setInterval` backstop.** Catches the cases where the observer is torn
  down by an SPA re-render, or the tab is throttled below the observer's
  delivery cadence.
- **Initial sample on script start.**

**No client-side dedup.** Identical-value observations are kept; the server stores
every row so plateau duration is preserved. Read-time rollups (collapsing runs of
identical values) are a query-side concern.

## Why `GM.xmlHttpRequest`, not `fetch()`

A plain `fetch()` from `https://claude.ai` to `http://localhost:27812` will fail in
modern browsers for **three** independent reasons:

- **Mixed content blocking**: HTTPS pages cannot make HTTP requests in page context.
  (Chrome treats `localhost` as a secure context for top-level navigation, but not
  as an exception to mixed-content rules for subresource requests in all cases.)
- **CORS preflight**: a cross-origin POST with `Content-Type: application/json` triggers
  a preflight that the trayapp would have to answer specifically.
- **Private Network Access (Chrome)**: requests from a public origin to a private
  network destination require an additional preflight with
  `Access-Control-Request-Private-Network: true`.

Tampermonkey/Violentmonkey provide `GM.xmlHttpRequest` (and the older `GM_xmlhttpRequest`)
which run in the extension's privileged context and bypass all three. The userscript
must use this and declare its allowed destinations:

```javascript
// ==UserScript==
// @match        https://claude.ai/*
// @grant        GM.xmlHttpRequest
// @connect      localhost
// @connect      127.0.0.1
// ==/UserScript==
```

Only this approach is guaranteed to work across Chrome, Firefox, and Edge.

## Endpoint

```
POST http://localhost:27812/snapshot
Content-Type: application/json

{
  "observed_at": "2026-04-25T17:32:14Z",
  "source": "userscript",
  "session_used": 6.0,
  "session_window_ends": "2026-04-25T19:02:11Z",
  "weekly_used": 23.0,
  "weekly_window_ends": "2026-04-30T06:00:00Z"
}
```

`session_used` and `weekly_used` are 0–100 percentages, both nullable: when only one
row is parseable the other field is omitted and the trayapp records what was found.
`*_window_ends` are RFC3339 timestamps derived from each row's "Resets …" hint;
they're omitted when the hint is in a format the parser doesn't recognize (e.g.
"Resets May 1" when the boundary is far enough out that Anthropic switches to a
date), in which case the server falls back to its calendar default.

## CORS

Because the userscript uses `GM.xmlHttpRequest` (which bypasses CORS), the trayapp does
**not** need to set CORS headers for the userscript path. CORS becomes relevant only if
the dashboard itself is ever loaded from an origin other than the trayapp's own server,
which is not currently planned.

## Failure handling

The userscript must:

- Not crash the page if the trayapp is unreachable. `GM.xmlHttpRequest` failures are
  swallowed with a console warning.
- Not double-post the same snapshot. Hash the relevant fields and skip if unchanged
  since the last successful post.
- Tolerate DOM changes. If the expected nodes are missing for >N seconds, post a
  `parse_error` payload to the local server (separate endpoint) so the trayapp can
  surface "userscript broke, please update" in the tray UI. The payload is a
  structured **fingerprint** (heading texts, progressbar counts, pathname) — not
  raw page HTML — so conversation content and account names never leave the
  browser.

## Distribution

A single file `userscript/claude-usage-snapshot.user.js` in the repo. The user installs
once via their userscript manager. Auto-update can be configured via `@updateURL` and
`@downloadURL` headers pointing at the file's GitHub raw URL — optional, off by default.

## Limitations (deliberate)

- Only fires when the user has the page open. This is not a bug; it's the design.
- Cannot read anything not visible in the DOM. If Anthropic moves quota detail to a
  separate page or behind a click, the script must be updated.
- Does not authenticate. The trayapp trusts any localhost POST; the threat model assumes
  the host is the trust boundary.
