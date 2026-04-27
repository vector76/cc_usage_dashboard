// ==UserScript==
// @name         Claude Usage Snapshot
// @namespace    https://github.com/vector76/cc_usage_dashboard
// @version      0.4.0
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
    // Backstop polling — primary signal is a MutationObserver on aria-valuenow,
    // so the interval only catches edge cases (observer torn down by SPA
    // re-render, tab woken from background throttle, etc.).
    const POST_INTERVAL_MS = 5 * 60 * 1000;
    const DOM_WAIT_TIMEOUT_MS = 30 * 1000;
    const DOM_MISSING_REPORT_MS = 5 * 60 * 1000;
    const PARSE_ERROR_REPORT_COOLDOWN_MS = 60 * 60 * 1000;

    // Section heading texts that anchor extraction. Row labels under each
    // heading change as Anthropic adjusts plan features (Sonnet only, Claude
    // Design, Routines, …); section names move much less. The first
    // progressbar following each heading is the one we keep.
    const SESSION_HEADING = 'Plan usage limits';
    const WEEKLY_HEADING = 'Weekly limits';

    // Coalesce burst mutations (multiple bars updating in one React commit)
    // into a single dispatch.
    const DISPATCH_DEBOUNCE_MS = 250;

    let lastParseErrorAt = 0;
    let domFirstMissingAt = null;
    let dispatchTimer = null;

    // ---------- utilities ----------

    function warn(...args) {
        try { console.warn('[claude-usage-snapshot]', ...args); } catch (_) { /* ignore */ }
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

    function onUsagePage() {
        return location.pathname === USAGE_PATH;
    }

    // ---------- DOM extraction ----------

    // For each progressbar, the most recent <h2> in document order tells us
    // which section it belongs to. This is robust to row-label edits and to
    // the order of sub-rows within a section.
    function precedingHeading(bar, headings) {
        let result = null;
        for (const h of headings) {
            if (h.node.compareDocumentPosition(bar) & Node.DOCUMENT_POSITION_FOLLOWING) {
                result = h.text;
            } else {
                break; // headings are in document order; stop at the first one not-before
            }
        }
        return result;
    }

    // Walk up from a progressbar to locate the row's reset hint. The label
    // column for each row carries a sibling <p> like "Resets in 19 min" or
    // "Resets Thu 11:00 PM"; we stop at the first ancestor that contains one.
    function findRowResetText(bar) {
        let node = bar.parentElement;
        for (let i = 0; i < 6 && node; i++, node = node.parentElement) {
            for (const p of node.querySelectorAll(':scope p')) {
                const t = (p.textContent || '').trim();
                if (/^Resets\b/i.test(t)) return t;
            }
        }
        return null;
    }

    // "Resets in 3 hr 33 min" / "Resets in 19 min" / "Resets in 5 hr".
    // baseMs is the wall-clock time the reset string was current — typically
    // Date.now() minus the page's "Last updated: N minutes ago" staleness, so
    // a stale page doesn't shift the computed end forward in time.
    function parseSessionEnds(text, baseMs) {
        if (!text) return null;
        const m = text.match(/Resets in\s+(?:(\d+)\s*hr)?\s*(?:(\d+)\s*min)?/i);
        if (!m) return null;
        const hours = parseInt(m[1] || '0', 10);
        const mins = parseInt(m[2] || '0', 10);
        if (hours === 0 && mins === 0) return null;
        return new Date(baseMs + (hours * 60 + mins) * 60 * 1000).toISOString();
    }

    // Parse the page's "Last updated" indicator into staleness in milliseconds.
    // The Anthropic page's progression is: "just now" → "less than a minute
    // ago" → "1 minute ago" → "N minutes ago" → "N hours ago" (long-idle
    // tabs). The first two collapse to 0; the rest are captured by the
    // numeric regex. Both the percent values and the "Resets in …" text are
    // accurate as of that timestamp, not as of Date.now(). Returns null when
    // the indicator can't be located, in which case the caller falls back to
    // treating the snapshot as current.
    function findLastUpdatedAgeMs() {
        const candidates = document.querySelectorAll('p, span, div');
        for (const node of candidates) {
            const t = (node.textContent || '').trim();
            // Skip large containers; we only want the small label itself.
            if (!t || t.length > 80) continue;
            if (!/last updated/i.test(t)) continue;
            if (/just now/i.test(t)) return 0;
            if (/less than a minute ago/i.test(t)) return 0;
            const m = t.match(/(\d+)\s*(minutes?|hours?)\s+ago/i);
            if (!m) continue;
            const n = parseInt(m[1], 10);
            const unit = m[2].toLowerCase();
            if (unit.startsWith('min')) return n * 60 * 1000;
            if (unit.startsWith('hour')) return n * 60 * 60 * 1000;
        }
        return null;
    }

    // "Resets Thu 11:00 PM" — weekday + time-of-day in the browser's local
    // timezone. We pick the next future occurrence of that weekday at that
    // local time and convert to UTC. Format variants like "Resets May 1"
    // (when far enough out that Anthropic switches to a date) are not
    // currently parsed; null falls back to the server's calendar default.
    const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
    function parseWeeklyEnds(text) {
        if (!text) return null;
        const m = text.match(/Resets\s+(Sun|Mon|Tue|Wed|Thu|Fri|Sat)[a-z]*\s+(\d{1,2}):(\d{2})\s*(AM|PM)/i);
        if (!m) return null;
        const targetDow = WEEKDAYS.indexOf(m[1].slice(0, 3));
        if (targetDow < 0) return null;
        const ampm = m[4].toUpperCase();
        let hour = parseInt(m[2], 10) % 12;
        if (ampm === 'PM') hour += 12;
        const min = parseInt(m[3], 10);

        const now = new Date();
        const target = new Date(now);
        target.setHours(hour, min, 0, 0);
        // Step forward until we hit the right weekday strictly in the future.
        for (let i = 0; i < 8; i++) {
            if (target.getDay() === targetDow && target > now) break;
            target.setDate(target.getDate() + 1);
        }
        return target.toISOString();
    }

    // Returns { sessionUsed, weeklyUsed, sessionWindowEnds, weeklyWindowEnds, observedAtMs }
    // or null when neither section yields a usable bar. observedAtMs is the
    // wall-clock time the page's numbers were accurate (Date.now() minus the
    // "Last updated" staleness, or Date.now() when the indicator is missing).
    function extractQuota() {
        const headings = Array.from(document.querySelectorAll('h2'))
            .map(h => ({ node: h, text: (h.textContent || '').trim() }));
        const bars = document.querySelectorAll('[role="progressbar"][aria-label="Usage"]');

        const ageMs = findLastUpdatedAgeMs();
        const observedAtMs = Date.now() - (ageMs || 0);

        let sessionUsed = null, weeklyUsed = null;
        let sessionEnds = null, weeklyEnds = null;

        for (const bar of bars) {
            const heading = precedingHeading(bar, headings);
            if (!heading) continue;

            const value = parseFloat(bar.getAttribute('aria-valuenow'));
            if (Number.isNaN(value)) continue;

            if (heading === SESSION_HEADING && sessionUsed === null) {
                sessionUsed = value;
                sessionEnds = parseSessionEnds(findRowResetText(bar), observedAtMs);
            } else if (heading === WEEKLY_HEADING && weeklyUsed === null) {
                weeklyUsed = value;
                // Weekly hint is an absolute clock time ("Resets Thu 11:00 PM"),
                // so page staleness doesn't shift it.
                weeklyEnds = parseWeeklyEnds(findRowResetText(bar));
            }
        }

        if (sessionUsed === null && weeklyUsed === null) return null;
        return { sessionUsed, weeklyUsed, sessionWindowEnds: sessionEnds, weeklyWindowEnds: weeklyEnds, observedAtMs };
    }

    // ---------- snapshot dispatch ----------

    function buildSnapshotBody(extracted) {
        const body = {
            observed_at: new Date(extracted.observedAtMs || Date.now()).toISOString(),
            source: 'userscript',
        };
        if (extracted.sessionUsed !== null) body.session_used = extracted.sessionUsed;
        if (extracted.weeklyUsed !== null) body.weekly_used = extracted.weeklyUsed;
        if (extracted.sessionWindowEnds) body.session_window_ends = extracted.sessionWindowEnds;
        if (extracted.weeklyWindowEnds) body.weekly_window_ends = extracted.weeklyWindowEnds;
        return body;
    }

    // No client-side dedup: identical-value observations are kept so plateau
    // duration is preserved. Server is responsible for any read-time rollup.
    function tryDispatch() {
        if (!onUsagePage()) {
            domFirstMissingAt = null;
            return;
        }

        const extracted = extractQuota();
        if (!extracted) {
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

        domFirstMissingAt = null;
        postJSON(ENDPOINT_SNAPSHOT, buildSnapshotBody(extracted));
    }

    function scheduleDispatch() {
        if (dispatchTimer) return;
        dispatchTimer = setTimeout(() => {
            dispatchTimer = null;
            tryDispatch();
        }, DISPATCH_DEBOUNCE_MS);
    }

    // ---------- change observer ----------

    // Body-level observer filtered to aria-valuenow attribute changes — fires
    // within milliseconds of claude.ai's poll updating the DOM, regardless of
    // tab focus or our setInterval phase. The attributeFilter keeps the
    // callback rate low even though subtree=true.
    function startChangeObserver() {
        const observer = new MutationObserver(mutations => {
            for (const m of mutations) {
                if (m.type !== 'attributes' || m.attributeName !== 'aria-valuenow') continue;
                const t = m.target;
                if (t && t.getAttribute && t.getAttribute('aria-label') === 'Usage') {
                    scheduleDispatch();
                    return;
                }
            }
        });
        observer.observe(document.body, {
            attributes: true,
            subtree: true,
            attributeFilter: ['aria-valuenow'],
        });
    }

    // ---------- DOM readiness ----------

    function waitForQuotaDOM(onReady) {
        let fired = false;
        const fire = () => {
            if (fired) return;
            fired = true;
            try { onReady(); } catch (e) { warn('onReady threw', e); }
        };

        const check = () => document.querySelector('[role="progressbar"][aria-label="Usage"]') !== null;
        if (check()) { fire(); return; }

        let observer = null;
        try {
            observer = new MutationObserver(() => {
                if (check()) {
                    observer.disconnect();
                    fire();
                }
            });
            observer.observe(document.documentElement, { childList: true, subtree: true });
        } catch (e) {
            warn('MutationObserver setup failed', e);
        }

        setTimeout(() => {
            if (observer) {
                try { observer.disconnect(); } catch (_) { /* ignore */ }
            }
            fire();
        }, DOM_WAIT_TIMEOUT_MS);
    }

    // ---------- bootstrap ----------

    function start() {
        // Initial sample, then hand the wheel to the change observer. The
        // interval is a backstop only — if the observer is somehow torn down
        // by an SPA re-render, or the tab is throttled, we still see a tick.
        tryDispatch();
        startChangeObserver();
        setInterval(tryDispatch, POST_INTERVAL_MS);
    }

    waitForQuotaDOM(start);
})();
