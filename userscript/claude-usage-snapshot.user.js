// ==UserScript==
// @name         Claude Usage Snapshot
// @namespace    https://github.com/vector76/cc_usage_dashboard
// @version      0.6.1
// @description  Reads "Current session" and "All models" usage % from claude.ai and posts them to the local Claude Usage Dashboard trayapp.
// @author       Claude Usage Dashboard
// @match        https://claude.ai/*
// @grant        GM.xmlHttpRequest
// @connect      localhost
// @connect      127.0.0.1
// @updateURL    https://raw.githubusercontent.com/vector76/cc_usage_dashboard/main/userscript/claude-usage-snapshot.user.js
// @downloadURL  https://raw.githubusercontent.com/vector76/cc_usage_dashboard/main/userscript/claude-usage-snapshot.user.js
// @run-at       document-start
// ==/UserScript==

(function () {
    'use strict';

    // ---------- visibility spoof (mirror of userscript/lib/visibility.js) ----------
    //
    // Installed first thing, before any other script reads the
    // visibility API. See lib/visibility.js for full rationale; the
    // short version is that claude.ai's poll loop pauses when the OS
    // reports the tab as hidden (screensaver, minimize), and we want
    // to keep it polling so the userscript still has fresh DOM to
    // observe. `@run-at document-start` (above) is what lets this
    // beat claude.ai's app init.
    function installVisibilitySpoof(doc) {
        if (!doc) return;

        function defineAlwaysVisible(propName, value) {
            try {
                Object.defineProperty(doc, propName, {
                    configurable: true,
                    get() { return value; },
                });
            } catch (_) {
                // Some hosts may have already locked the property
                // as non-configurable; silently no-op.
            }
        }

        defineAlwaysVisible('hidden', false);
        defineAlwaysVisible('visibilityState', 'visible');
        defineAlwaysVisible('webkitHidden', false);
        defineAlwaysVisible('webkitVisibilityState', 'visible');

        // Suppress visibilitychange events at the capture phase
        // before any application-registered listener on `document`
        // can observe them. (visibilitychange does not bubble to
        // window, so listeners there are out of scope.) Cover the
        // prefixed variant too.
        if (typeof doc.addEventListener === 'function') {
            const swallow = (e) => {
                if (typeof e.stopImmediatePropagation === 'function') {
                    e.stopImmediatePropagation();
                }
                if (typeof e.stopPropagation === 'function') {
                    e.stopPropagation();
                }
            };
            doc.addEventListener('visibilitychange', swallow, true);
            doc.addEventListener('webkitvisibilitychange', swallow, true);
        }
    }
    installVisibilitySpoof(typeof document !== 'undefined' ? document : null);

    const ENDPOINT_SNAPSHOT = 'http://localhost:27812/snapshot';
    const ENDPOINT_PARSE_ERROR = 'http://localhost:27812/parse_error';

    const USAGE_PATH = '/settings/usage';
    // Backstop polling — primary signal is a MutationObserver on aria-valuenow,
    // so the interval only catches edge cases (observer torn down by SPA
    // re-render, tab woken from background throttle, etc.). Each tick is
    // gated by the dedup decision, so a fast cadence is cheap.
    const POST_INTERVAL_MS = 60 * 1000;
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

    // Rolling reference for the limbo "Last updated decrease" trigger.
    // Updated on every DOM read regardless of whether we sent, because
    // the staleness counter can roll back (claude.ai re-poll) between
    // two successive reads even when nothing else changes — and because
    // anchoring this to last-sent state self-traps once an "age=0" send
    // lands. Lost on page reload; cold-start handles re-establishment.
    let lastObservedAgeMs = null;

    // ---------- persistent state ----------

    // Mirror of userscript/lib/state.js — same source of truth, inlined
    // here so Tampermonkey runs without a build step. Edit both together.
    const STATE_STORAGE_KEY = 'claude-usage-snapshot.state.v1';

    function loadState() {
        try {
            const storage = (typeof globalThis !== 'undefined' && globalThis.localStorage) || null;
            if (!storage) return null;
            const raw = storage.getItem(STATE_STORAGE_KEY);
            if (raw == null) return null;
            const parsed = JSON.parse(raw);
            if (!parsed || typeof parsed !== 'object') return null;
            if (typeof parsed.lastSentAtMs !== 'number') return null;
            const result = {
                lastSentAtMs: parsed.lastSentAtMs,
                lastPercent: parsed.lastPercent,
                lastResetText: parsed.lastResetText,
                lastWindowEndsMs: parsed.lastWindowEndsMs,
            };
            if (parsed.lastSessionActive !== undefined) result.lastSessionActive = parsed.lastSessionActive;
            return result;
        } catch (_) {
            return null;
        }
    }

    function recordSentState({ sentAtMs, percent, resetText, windowEndsMs, sessionActive }) {
        try {
            const storage = (typeof globalThis !== 'undefined' && globalThis.localStorage) || null;
            if (!storage) return;
            const record = {
                lastSentAtMs: sentAtMs,
                lastPercent: percent,
                lastResetText: resetText,
                lastWindowEndsMs: windowEndsMs,
            };
            if (sessionActive !== undefined) record.lastSessionActive = sessionActive;
            storage.setItem(STATE_STORAGE_KEY, JSON.stringify(record));
        } catch (_) {
            // Persistence is best-effort.
        }
    }

    // ---------- continuity decision (mirror of userscript/lib/continuity.js) ----------

    const WALL_CLOCK_GAP_MS = 15 * 60 * 1000;
    const WINDOW_ENDS_JUMP_MS = 60 * 60 * 1000;

    function decideContinuity(observation, prevState, nowMs) {
        if (!prevState) return false;

        if (nowMs - prevState.lastSentAtMs > WALL_CLOCK_GAP_MS) return false;

        if (observation.percent < prevState.lastPercent) return false;

        const cur = observation.windowEndsMs;
        const prev = prevState.lastWindowEndsMs;
        if (typeof cur === 'number' && typeof prev === 'number' &&
            Math.abs(cur - prev) > WINDOW_ENDS_JUMP_MS) {
            return false;
        }

        return true;
    }

    // ---------- dedup decision (mirror of userscript/lib/dedup.js) ----------

    // The parameter intentionally shadows the module-level
    // `lastObservedAgeMs` so the body is textually identical to the
    // single source of truth in userscript/lib/dedup.js; the shadow is
    // local to this function and the module-level binding is unchanged.
    function shouldSend(observation, prevState, lastObservedAgeMs) {
        if (!prevState) return 'send';

        if (observation.sessionUsed !== prevState.lastPercent) return 'send';

        if (observation.resetText !== prevState.lastResetText) return 'send';

        const wasLimbo = prevState.lastSessionActive === false;
        const nowLimbo = observation.sessionActive === false;
        if (wasLimbo !== nowLimbo) return 'send';

        if (nowLimbo) {
            // While in limbo the visible numbers don't move, so a strict
            // *decrease* in "Last updated" age is our only signal that a
            // fresh poll landed. Compare against the rolling
            // most-recently-observed age (not the persisted last-sent
            // age, which would self-trap at its floor of 0). Null on
            // either side is "no information" and must not fire. We do
            // NOT fire on the age incrementing — that advances on pure
            // wall-clock time and would re-introduce the spam dedup is
            // meant to prevent.
            const cur = observation.lastUpdatedAgeMs;
            if (cur != null && lastObservedAgeMs != null && cur < lastObservedAgeMs) {
                return 'send';
            }
        }

        return 'skip';
    }

    // ---------- utilities ----------

    function warn(...args) {
        try { console.warn('[claude-usage-snapshot]', ...args); } catch (_) { /* ignore */ }
    }

    function postJSON(url, body, onSuccess) {
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
                        return;
                    }
                    if (typeof onSuccess === 'function') {
                        try { onSuccess(); } catch (e) { warn('onSuccess threw', e); }
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

    // Detect the "no active session" limbo label on the session row. When no
    // session window is open, Anthropic replaces the "Resets in …" hint with
    // copy like "Starts when a message is sent". We scope the walk to the
    // session row's ancestors (same shape as findRowResetText) so similar
    // marketing/help text elsewhere on the page can't trigger a false match.
    function isSessionLimbo(bar) {
        const needle = 'starts when a message is sent';
        let node = bar.parentElement;
        for (let i = 0; i < 6 && node; i++, node = node.parentElement) {
            for (const p of node.querySelectorAll(':scope p')) {
                const t = (p.textContent || '').toLowerCase();
                if (t.includes(needle)) return true;
            }
        }
        return false;
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

    // Returns { sessionUsed, weeklyUsed, sessionWindowEnds, weeklyWindowEnds,
    //           sessionActive, observedAtMs, resetText, lastUpdatedAgeMs }
    // or null when neither section yields a usable bar. observedAtMs is the
    // wall-clock time the page's numbers were accurate (Date.now() minus the
    // "Last updated" staleness, or Date.now() when the indicator is missing).
    // sessionActive is false only when the limbo label is positively detected;
    // it is left undefined otherwise (we never assert it is true). resetText
    // is the verbatim "Resets in …" text on the session row (null in limbo or
    // when missing), used by the dedup layer to spot string ticks. lastUpdatedAgeMs
    // is the raw "Last updated" staleness in ms (null when unparsable).
    function extractQuota() {
        // Anthropic moved the section headings from <h2> to <h3> as of late
        // April 2026 and started appending plan-tier badges to the heading
        // text (e.g. "Plan usage limitsMax (20x)"). We accept either tag and
        // match the section name as a *prefix* so a trailing badge doesn't
        // break extraction.
        const headings = Array.from(document.querySelectorAll('h2, h3'))
            .map(h => ({ node: h, text: (h.textContent || '').trim() }));
        const bars = document.querySelectorAll('[role="progressbar"][aria-label="Usage"]');

        const lastUpdatedAgeMs = findLastUpdatedAgeMs();
        const observedAtMs = Date.now() - (lastUpdatedAgeMs || 0);

        let sessionUsed = null, weeklyUsed = null;
        let sessionEnds = null, weeklyEnds = null;
        let sessionActive;
        let sessionResetText = null;

        for (const bar of bars) {
            const heading = precedingHeading(bar, headings);
            if (!heading) continue;

            const value = parseFloat(bar.getAttribute('aria-valuenow'));
            if (Number.isNaN(value)) continue;

            if (heading.startsWith(SESSION_HEADING) && sessionUsed === null) {
                sessionUsed = value;
                sessionResetText = findRowResetText(bar);
                sessionEnds = parseSessionEnds(sessionResetText, observedAtMs);
                if (isSessionLimbo(bar)) sessionActive = false;
            } else if (heading.startsWith(WEEKLY_HEADING) && weeklyUsed === null) {
                weeklyUsed = value;
                // Weekly hint is an absolute clock time ("Resets Thu 11:00 PM"),
                // so page staleness doesn't shift it.
                weeklyEnds = parseWeeklyEnds(findRowResetText(bar));
            }
        }

        if (sessionUsed === null && weeklyUsed === null) return null;
        return {
            sessionUsed,
            weeklyUsed,
            sessionWindowEnds: sessionEnds,
            weeklyWindowEnds: weeklyEnds,
            sessionActive,
            observedAtMs,
            resetText: sessionResetText,
            lastUpdatedAgeMs,
        };
    }

    // ---------- diagnostics ----------

    // buildFingerprint summarises the *structure* of the page when our
    // extractor breaks, without including conversation text, account
    // names, or any other PII. Earlier versions shipped up to 64 KiB of
    // document.body.outerHTML; that landed verbatim in parse_errors and
    // sat on disk for 30 days. The fingerprint captures what an admin
    // actually needs to debug a parser break (which selectors matched
    // how many times, what the section headings look like) and nothing
    // else.
    function buildFingerprint() {
        try {
            const headings = Array.from(document.querySelectorAll('h2'))
                .map(h => (h.textContent || '').trim().slice(0, 80))
                .filter(Boolean)
                .slice(0, 30);
            const fp = {
                pathname: location.pathname,
                h2_count: headings.length,
                h2_texts: headings,
                progressbar_count: document.querySelectorAll('[role="progressbar"]').length,
                usage_progressbar_count: document.querySelectorAll('[role="progressbar"][aria-label="Usage"]').length,
                user_agent_short: (navigator.userAgent || '').slice(0, 120),
            };
            return JSON.stringify(fp);
        } catch (e) {
            return JSON.stringify({ fingerprint_error: String(e).slice(0, 200) });
        }
    }

    // ---------- snapshot dispatch ----------

    function buildSnapshotBody(extracted, continuousWithPrev) {
        const body = {
            observed_at: new Date(extracted.observedAtMs || Date.now()).toISOString(),
            source: 'userscript',
            continuous_with_prev: continuousWithPrev,
        };
        if (extracted.sessionUsed !== null) body.session_used = extracted.sessionUsed;
        if (extracted.weeklyUsed !== null) body.weekly_used = extracted.weeklyUsed;
        if (extracted.sessionWindowEnds) body.session_window_ends = extracted.sessionWindowEnds;
        if (extracted.weeklyWindowEnds) body.weekly_window_ends = extracted.weeklyWindowEnds;
        // Limbo signal: only emit when positively detected. We never assert
        // session_active=true — absence of the field means "unknown".
        if (extracted.sessionActive === false) body.session_active = false;
        return body;
    }

    // Freshness-driven dedup: emit only when at least one meaningful-change
    // signal has fired since the last successful send. The decision lives in
    // shouldSend(); see lib/dedup.js for the canonical logic and rationale.
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
                    payload: buildFingerprint(),
                });
            }
            return;
        }

        domFirstMissingAt = null;

        const prevState = loadState();
        const decision = shouldSend(extracted, prevState, lastObservedAgeMs);

        // Update the rolling observed-age *after* the comparison, so
        // the next call sees this read as "previous." Update on every
        // read regardless of decision; otherwise we'd never detect a
        // staleness rollback during a long limbo plateau.
        if (extracted.lastUpdatedAgeMs != null) {
            lastObservedAgeMs = extracted.lastUpdatedAgeMs;
        }

        if (decision === 'skip') return;

        const windowEndsMs = extracted.sessionWindowEnds ? Date.parse(extracted.sessionWindowEnds) : null;
        const nowMs = Date.now();
        const continuousWithPrev = decideContinuity(
            {
                percent: extracted.sessionUsed,
                resetText: extracted.resetText,
                windowEndsMs,
                sessionActive: extracted.sessionActive,
                observedAtMs: extracted.observedAtMs,
            },
            prevState,
            nowMs,
        );

        postJSON(ENDPOINT_SNAPSHOT, buildSnapshotBody(extracted, continuousWithPrev), () => {
            recordSentState({
                sentAtMs: Date.now(),
                percent: extracted.sessionUsed,
                resetText: extracted.resetText,
                windowEndsMs,
                sessionActive: extracted.sessionActive,
            });
        });
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
