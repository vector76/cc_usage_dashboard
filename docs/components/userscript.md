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

When loaded on a `claude.ai/*` page that exposes quota numbers:

1. Wait for the relevant DOM nodes to appear (MutationObserver, with a sane timeout).
2. Extract:
   - 5-hour remaining (and total/end time if shown).
   - Weekly remaining (and total/end time if shown).
3. POST to `http://localhost:27812/snapshot` with a JSON body.
4. Re-fire on a debounced interval (e.g. every 60s) while the page remains open, so a
   user who leaves the tab open all day generates many cheap snapshots.

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
  "five_hour_remaining": 64.2,
  "five_hour_total": 100.0,
  "five_hour_window_ends": "2026-04-25T19:02:11Z",
  "weekly_remaining": 1380.4,
  "weekly_total": 2000.0,
  "weekly_window_ends": "2026-04-28T00:00:00Z",
  "raw_dom_text": "..."
}
```

`raw_dom_text` is included as a forensic record so we can fix parser regressions without
losing data: the trayapp stores the raw text in `quota_snapshots.raw_json` even when
parsing fails.

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
  surface "userscript broke, please update" in the tray UI.

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
