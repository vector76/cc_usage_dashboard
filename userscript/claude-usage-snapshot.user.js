// ==UserScript==
// @name         Claude Usage Snapshot
// @namespace    https://github.com/vector76/cc_usage_dashboard
// @version      0.2.0
// @description  Reads "Current session" and "All models" usage % from claude.ai and posts them to the local Claude Usage Dashboard trayapp.
// @author       Claude Usage Dashboard
// @match        https://claude.ai/*
// @grant        GM.xmlHttpRequest
// @connect      localhost
// @connect      127.0.0.1
// @updateURL    https://raw.githubusercontent.com/vector76/cc_usage_dashboard/main/userscript/claude-usage-snapshot.user.js
// @downloadURL  https://raw.githubusercontent.com/vector76/cc_usage_dashboard/main/userscript/claude-usage-snapshot.user.js
// @run-at       document-idle
// ==/UserScript==

(function () {
    'use strict';

    const ENDPOINT_SNAPSHOT = 'http://localhost:27812/snapshot';
    const ENDPOINT_PARSE_ERROR = 'http://localhost:27812/parse_error';

    const USAGE_PATH = '/settings/usage';
    const POST_INTERVAL_MS = 60 * 1000;          // debounce snapshots to once per minute
    const DOM_WAIT_TIMEOUT_MS = 30 * 1000;       // how long to wait for quota nodes to appear
    const DOM_MISSING_REPORT_MS = 5 * 60 * 1000; // report parse_error after 5min of missing DOM
    const PARSE_ERROR_REPORT_COOLDOWN_MS = 60 * 60 * 1000; // don't spam parse_error more than 1/hr

    // Exact label strings shown in the Claude usage page row labels. Anything
    // else ("Sonnet only", "Claude Design", "Daily included routine runs", …)
    // is intentionally ignored.
    const LABEL_SESSION = 'Current session';
    const LABEL_WEEKLY = 'All models';

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

    // True iff we're on the page that actually exposes quota numbers. Gating
    // by URL keeps us from posting parse_error payloads that contain private
    // chat HTML when the user is on /chat/* or other routes.
    function onUsagePage() {
        return location.pathname === USAGE_PATH;
    }

    // Walk up from a progressbar node to the row container that holds the
    // label <p>. Layout (as of 2026-04): each row is a flex-row with a
    // [w-13rem] label column on the left and the bar+percent on the right.
    // We find the nearest ancestor that contains both a <p> sibling and the
    // bar — concretely, any ancestor whose textContent contains the label
    // strings we care about.
    function findRowLabel(progressbar) {
        let node = progressbar.parentElement;
        // Bound the climb so we don't scan the whole document if the layout shifts.
        for (let i = 0; i < 6 && node; i++, node = node.parentElement) {
            // Look for a <p> child with one of our exact label strings. We
            // restrict to <p> to avoid matching "Current session" embedded in
            // arbitrary helper text.
            const ps = node.querySelectorAll(':scope p');
            for (const p of ps) {
                const text = (p.textContent || '').trim();
                if (text === LABEL_SESSION || text === LABEL_WEEKLY) {
                    return text;
                }
            }
        }
        return null;
    }

    // Returns { sessionUsed, weeklyUsed } where each is a 0–100 number or null.
    // Reads structured progressbar nodes rather than text-scraping, so it's
    // robust to copy changes that don't touch the row labels.
    function extractQuota() {
        const bars = document.querySelectorAll('[role="progressbar"][aria-label="Usage"]');
        let sessionUsed = null;
        let weeklyUsed = null;

        for (const bar of bars) {
            const label = findRowLabel(bar);
            if (!label) continue;
            const valueStr = bar.getAttribute('aria-valuenow');
            if (valueStr == null) continue;
            const value = parseFloat(valueStr);
            if (Number.isNaN(value)) continue;
            if (label === LABEL_SESSION && sessionUsed === null) {
                sessionUsed = value;
            } else if (label === LABEL_WEEKLY && weeklyUsed === null) {
                weeklyUsed = value;
            }
        }

        if (sessionUsed === null && weeklyUsed === null) return null;
        return { sessionUsed, weeklyUsed };
    }

    // ---------- snapshot dispatch ----------

    function buildSnapshotBody(extracted) {
        const body = {
            observed_at: new Date().toISOString(),
            source: 'userscript',
        };
        if (extracted.sessionUsed !== null) {
            body.session_used = extracted.sessionUsed;
        }
        if (extracted.weeklyUsed !== null) {
            body.weekly_used = extracted.weeklyUsed;
        }
        return body;
    }

    function tryDispatch() {
        // Skip everything when not on the usage page — chat threads, project
        // pages etc. never expose these progressbars and we don't want to
        // accidentally ship private DOM via parse_error.
        if (!onUsagePage()) {
            domFirstMissingAt = null;
            return;
        }

        const extracted = extractQuota();
        if (!extracted) {
            // We're on the usage page but couldn't find either bar. Sustained
            // misses indicate the DOM has shifted in a way we don't recognize.
            if (domFirstMissingAt === null) domFirstMissingAt = Date.now();
            const missingFor = Date.now() - domFirstMissingAt;
            if (missingFor > DOM_MISSING_REPORT_MS &&
                Date.now() - lastParseErrorAt > PARSE_ERROR_REPORT_COOLDOWN_MS) {
                lastParseErrorAt = Date.now();
                postJSON(ENDPOINT_PARSE_ERROR, {
                    source: 'userscript',
                    reason: 'usage progressbars missing for >5 minutes',
                    payload: (document.body && document.body.outerHTML) ?
                        document.body.outerHTML.slice(0, 65536) : '',
                });
            }
            return;
        }

        // DOM is parseable again; reset the missing-timer.
        domFirstMissingAt = null;

        const body = buildSnapshotBody(extracted);

        // Hash the meaningful fields (exclude observed_at) so DOM whitespace
        // churn doesn't defeat de-dup.
        const hashKey = JSON.stringify({
            s: body.session_used,
            w: body.weekly_used,
        });
        const h = hashString(hashKey);
        if (h === lastPayloadHash) return;
        lastPayloadHash = h;
        postJSON(ENDPOINT_SNAPSHOT, body);
    }

    // ---------- DOM readiness ----------

    // Wait up to DOM_WAIT_TIMEOUT_MS for the SPA to render at least one
    // [role=progressbar][aria-label=Usage] node, then start the periodic
    // poller. The MutationObserver lets us react quickly when the SPA
    // navigates to /settings/usage after initial load.
    function waitForQuotaDOM(onReady) {
        let fired = false;
        const fire = () => {
            if (fired) return;
            fired = true;
            try { onReady(); } catch (e) { warn('onReady threw', e); }
        };

        const check = () => {
            return document.querySelector('[role="progressbar"][aria-label="Usage"]') !== null;
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
                childList: true, subtree: true,
            });
        } catch (e) {
            warn('MutationObserver setup failed', e);
        }

        // Hard ceiling: start the poller anyway after the timeout so SPA
        // navigations to the usage page after this load still get sampled.
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
        // 60s interval catches SPA navigations to /settings/usage within the
        // same tab; we deliberately don't reset dedup state on URL change
        // since identical values across re-visits shouldn't generate
        // duplicate snapshots.
        tryDispatch();
        setInterval(tryDispatch, POST_INTERVAL_MS);
    }

    waitForQuotaDOM(start);
})();
