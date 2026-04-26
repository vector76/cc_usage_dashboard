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
2. For each progressbar, walk up to the row container and identify the row by its
   first `<p>` label. Only two labels are kept; everything else is ignored:
   - **Current session** → `session_used` (% of the rolling 5-hour window).
   - **All models** → `weekly_used` (% of the weekly limit).
   "Sonnet only", "Claude Design", "Daily included routine runs", and the extra-usage
   bars are explicitly skipped — they are not part of the headline plan-limit signal.
3. Read each bar's `aria-valuenow` (a 0–100 number) directly. We do not text-scrape
   the "X% used" label, since the DOM exposes the value in a structured attribute.
4. POST to `http://localhost:27812/snapshot` with a JSON body.
5. Re-fire on a debounced interval (every 60s) while the tab is open. The dedup hash
   is based only on the two percentages, so re-posts from the same state are silently
   dropped.

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
  "weekly_used": 23.0
}
```

`session_used` and `weekly_used` are 0–100 percentages, both nullable: when only one
row is parseable the other field is omitted and the trayapp records what was found.
`*_window_ends` fields are accepted by the server schema and reserved for future use
(the userscript does not currently parse the "Resets in 3 hr 33 min" / "Resets Thu
11:00 PM" hints).

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
