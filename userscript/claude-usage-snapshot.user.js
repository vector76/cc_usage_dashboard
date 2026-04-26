// ==UserScript==
// @name         Claude Usage Snapshot
// @namespace    https://github.com/anthropics/usage-dashboard
// @version      0.1.0
// @description  Reads quota numbers from claude.ai and posts them to the local Claude Usage Dashboard trayapp.
// @author       Claude Usage Dashboard
// @match        https://claude.ai/*
// @grant        GM.xmlHttpRequest
// @connect      localhost
// @connect      127.0.0.1
// @updateURL    https://raw.githubusercontent.com/anthropics/usage-dashboard/main/userscript/claude-usage-snapshot.user.js
// @downloadURL  https://raw.githubusercontent.com/anthropics/usage-dashboard/main/userscript/claude-usage-snapshot.user.js
// @run-at       document-idle
// ==/UserScript==

(function () {
    'use strict';

    const ENDPOINT_SNAPSHOT = 'http://localhost:27812/snapshot';
    const ENDPOINT_PARSE_ERROR = 'http://localhost:27812/parse_error';

    const POST_INTERVAL_MS = 60 * 1000;          // debounce snapshots to once per minute
    const DOM_WAIT_TIMEOUT_MS = 30 * 1000;       // how long to wait for quota nodes to appear
    const DOM_MISSING_REPORT_MS = 5 * 60 * 1000; // report parse_error after 5min of missing DOM
    const PARSE_ERROR_REPORT_COOLDOWN_MS = 60 * 60 * 1000; // don't spam parse_error more than 1/hr

    let lastPayloadHash = null;
    let lastParseErrorAt = 0;
    let domFirstMissingAt = null;

    // ---------- utilities ----------

    function warn(...args) {
        try { console.warn('[claude-usage-snapshot]', ...args); } catch (_) { /* ignore */ }
    }

    // djb2 hash; tiny and good enough for change detection.
    function hashString(s) {
        let h = 5381;
        for (let i = 0; i < s.length; i++) {
            h = ((h << 5) + h) + s.charCodeAt(i);
            h = h | 0;
        }
        return (h >>> 0).toString(16);
    }

    function postJSON(url, body) {
        try {
            const payload = JSON.stringify(body);
            GM.xmlHttpRequest({
                method: 'POST',
                url: url,
                headers: { 'Content-Type': 'application/json' },
                data: payload,
                timeout: 5000,
                onerror: (e) => warn('POST failed', url, e && e.error),
                ontimeout: () => warn('POST timed out', url),
                onload: (resp) => {
                    if (resp.status < 200 || resp.status >= 300) {
                        warn('POST non-2xx', url, resp.status);
                    }
                },
            });
        } catch (e) {
            warn('POST threw', url, e);
        }
    }

    // ---------- DOM extraction ----------

    // Word-boundary checks so unrelated text like "25h" or "weekday" doesn't
    // trip the quota-page heuristic.
    const FIVE_HOUR_RE = /(?:^|\W)(?:5-hour|5\s*hour|five-hour|5h)\b/i;
    const WEEKLY_RE = /(?:^|\W)week(?:ly|s)?\b/i;

    // True iff the page text looks like it *should* expose quota numbers.
    // Used to gate parse_error reporting so we don't flag chat threads (which
    // never show quota) and don't ship private chat HTML to the trayapp.
    function pageLooksLikeQuotaView(text) {
        if (!/remaining/i.test(text)) return false;
        return FIVE_HOUR_RE.test(text) || WEEKLY_RE.test(text);
    }

    // Look up to LABEL_VALUE_LOOKAHEAD chars after a label for the next "N%".
    // Used by the cross-line fallback when the SPA renders the label and
    // percentage on separate lines (e.g. stacked typography).
    const LABEL_VALUE_LOOKAHEAD = 120;

    function findPercentNear(text, labelRe) {
        const m = labelRe.exec(text);
        if (!m) return null;
        const start = m.index + m[0].length;
        const lookahead = text.slice(start, start + LABEL_VALUE_LOOKAHEAD);
        const pm = lookahead.match(/(\d+(?:\.\d+)?)\s*%/);
        if (!pm) return null;
        const v = parseFloat(pm[1]);
        return Number.isNaN(v) ? null : v;
    }

    function extractQuota(text) {
        if (!text) return null;

        // Pass 1 — line-based: classify each percentage line by labels in the
        // same line. Cheapest and most precise when the UI renders
        // "5-hour usage: 64% remaining" as a single line.
        // Note: we deliberately do NOT try to parse "resets in 3d 4h" into a
        // window_ends timestamp here. The server expects RFC3339 in
        // *_window_ends and will reject the whole payload if we send anything
        // else. raw_dom_text preserves the original phrasing for later use.
        let fiveHour = null;
        let weekly = null;
        const lines = text.split(/\r?\n/);
        for (let i = 0; i < lines.length; i++) {
            const line = lines[i];
            const pctMatch = line.match(/(\d+(?:\.\d+)?)\s*%/);
            if (!pctMatch) continue;
            const pct = parseFloat(pctMatch[1]);
            if (Number.isNaN(pct)) continue;

            if (FIVE_HOUR_RE.test(line) && fiveHour === null) {
                fiveHour = pct;
            } else if (WEEKLY_RE.test(line) && weekly === null) {
                weekly = pct;
            }
        }

        // Pass 2 — cross-line fallback: if Pass 1 missed a value, look at
        // text that follows the label by up to LABEL_VALUE_LOOKAHEAD chars.
        if (fiveHour === null) fiveHour = findPercentNear(text, FIVE_HOUR_RE);
        if (weekly === null) weekly = findPercentNear(text, WEEKLY_RE);

        if (fiveHour === null && weekly === null) return null;

        return {
            fiveHourRemaining: fiveHour,
            weeklyRemaining: weekly,
            rawDomText: text.slice(0, 8192), // bound the payload size
        };
    }

    // ---------- snapshot dispatch ----------

    function buildSnapshotBody(extracted) {
        const body = {
            observed_at: new Date().toISOString(),
            source: 'userscript',
            raw_dom_text: extracted.rawDomText,
        };
        if (extracted.fiveHourRemaining !== null) {
            body.five_hour_remaining = extracted.fiveHourRemaining;
            body.five_hour_total = 100.0;
        }
        if (extracted.weeklyRemaining !== null) {
            body.weekly_remaining = extracted.weeklyRemaining;
            body.weekly_total = 100.0;
        }
        return body;
    }

    function tryDispatch() {
        const text = (document.body && document.body.innerText) || '';

        // If this isn't a quota-bearing page (e.g. a chat thread), don't
        // count it as "DOM missing" — that would leak private chat HTML
        // to the trayapp via the parse_error path.
        if (!pageLooksLikeQuotaView(text)) {
            domFirstMissingAt = null;
            return;
        }

        const extracted = extractQuota(text);
        if (!extracted) {
            // Page looks like it should have quota but we couldn't parse it.
            // This is a real parse error worth reporting after a sustained miss.
            if (domFirstMissingAt === null) domFirstMissingAt = Date.now();
            const missingFor = Date.now() - domFirstMissingAt;
            if (missingFor > DOM_MISSING_REPORT_MS &&
                Date.now() - lastParseErrorAt > PARSE_ERROR_REPORT_COOLDOWN_MS) {
                lastParseErrorAt = Date.now();
                postJSON(ENDPOINT_PARSE_ERROR, {
                    source: 'userscript',
                    reason: 'quota DOM nodes missing for >5 minutes',
                    payload: (document.body && document.body.outerHTML) ?
                        document.body.outerHTML.slice(0, 65536) : '',
                });
            }
            return;
        }

        // DOM is parseable again; reset the missing-timer.
        domFirstMissingAt = null;

        const body = buildSnapshotBody(extracted);

        // Hash the meaningful fields (exclude observed_at and raw_dom_text so
        // that DOM whitespace churn doesn't defeat de-dup).
        const hashKey = JSON.stringify({
            f: body.five_hour_remaining,
            w: body.weekly_remaining,
        });
        const h = hashString(hashKey);
        if (h === lastPayloadHash) return;
        lastPayloadHash = h;
        postJSON(ENDPOINT_SNAPSHOT, body);
    }

    // ---------- DOM readiness ----------

    // Wait up to DOM_WAIT_TIMEOUT_MS for the page to contain text matching our
    // heuristic, then start the periodic poller. The MutationObserver lets us
    // react quickly when the SPA renders the quota panel after navigation.
    function waitForQuotaDOM(onReady) {
        let fired = false;
        const fire = () => {
            if (fired) return;
            fired = true;
            try { onReady(); } catch (e) { warn('onReady threw', e); }
        };

        const check = () => {
            const t = (document.body && document.body.innerText) || '';
            return /\d+(?:\.\d+)?\s*%/.test(t) || /remaining/i.test(t);
        };

        if (check()) { fire(); return; }

        let observer = null;
        try {
            observer = new MutationObserver(() => {
                if (check()) {
                    observer.disconnect();
                    fire();
                }
            });
            observer.observe(document.documentElement, {
                childList: true, subtree: true, characterData: true,
            });
        } catch (e) {
            warn('MutationObserver setup failed', e);
        }

        // Hard ceiling: start the poller anyway after the timeout.
        // tryDispatch will handle the "DOM missing" reporting path.
        setTimeout(() => {
            if (observer) {
                try { observer.disconnect(); } catch (_) { /* ignore */ }
            }
            fire();
        }, DOM_WAIT_TIMEOUT_MS);
    }

    // ---------- bootstrap ----------

    function start() {
        // First attempt immediately; then debounce to once per minute. The
        // 60s interval catches SPA navigations to new views within the same
        // window; we deliberately don't reset dedup state on URL change since
        // identical values across views shouldn't generate duplicate snapshots.
        tryDispatch();
        setInterval(tryDispatch, POST_INTERVAL_MS);
    }

    waitForQuotaDOM(start);
})();
