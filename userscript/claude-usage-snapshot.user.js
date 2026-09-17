// ==UserScript==
// @name         Claude Usage Snapshot
// @namespace    https://github.com/vector76/cc_usage_dashboard
// @version      0.10.0
// @description  Reads "Current session", "This week" (formerly "All models"), and "Fable" usage % from claude.ai and posts them to the local Claude Usage Dashboard trayapp.
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

    // Legacy full-page route. As of the June 2026 redesign, settings is a
    // hash-routed modal ("/new#settings/usage"); see isUsageRoute (mirror of
    // userscript/lib/route.js) for the predicate that accepts both forms.
    const USAGE_PATH = '/settings/usage';
    // Backstop polling — primary signal is a MutationObserver on aria-valuenow,
    // so the interval only catches edge cases (observer torn down by SPA
    // re-render, tab woken from background throttle, etc.). Each tick is
    // gated by the dedup decision, so a fast cadence is cheap.
    const POST_INTERVAL_MS = 60 * 1000;
    const DOM_WAIT_TIMEOUT_MS = 30 * 1000;
    const DOM_MISSING_REPORT_MS = 5 * 60 * 1000;
    const PARSE_ERROR_REPORT_COOLDOWN_MS = 60 * 60 * 1000;

    // ---------- section and row recognition (mirror of userscript/lib/rows.js) ----------
    //
    // See lib/rows.js for the full rationale. Section headings anchor
    // extraction and are matched as a prefix so a trailing plan-tier badge
    // ("Your usage limitsTeam", "Plan usage limitsMax (20x)") doesn't break
    // it. In the legacy two-section layout the first bar under each heading
    // is the one we keep (plus the Fable sub-row by label); in the
    // September 2026 combined "Your usage" section every row is claimed by
    // its label. Edit both copies together; test/inline-drift.test.js
    // enforces that they agree.
    //
    // Known heading history:
    //   "Plan usage limits" + "Weekly limits" — through April 2026
    //   "Your usage limits" + "Weekly limits" — May 2026 through August 2026
    //   "Your usage" (combined)               — observed September 2026
    const FABLE_ROW_LABEL_PREFIXES = ['fable'];
    const SESSION_ROW_LABEL_PREFIXES = ['current session'];
    const WEEKLY_ROW_LABEL_PREFIXES = ['this week', 'all models'];
    const SESSION_HEADINGS = ['Your usage limits', 'Plan usage limits'];
    const WEEKLY_HEADINGS = ['Weekly limits'];
    const COMBINED_HEADINGS = ['Your usage'];

    function _hasPrefix(text, prefixes) {
        if (typeof text !== 'string') return false;
        const t = text.trim().toLowerCase();
        if (!t) return false;
        return prefixes.some(p => t.startsWith(p.toLowerCase()));
    }

    function isFableRowLabel(label) {
        return _hasPrefix(label, FABLE_ROW_LABEL_PREFIXES);
    }

    function classifyUsageRow(label) {
        if (isFableRowLabel(label)) return 'fable';
        if (_hasPrefix(label, SESSION_ROW_LABEL_PREFIXES)) return 'session';
        if (_hasPrefix(label, WEEKLY_ROW_LABEL_PREFIXES)) return 'weekly';
        return null;
    }

    function classifySection(heading) {
        if (_hasPrefix(heading, SESSION_HEADINGS)) return 'session';
        if (_hasPrefix(heading, WEEKLY_HEADINGS)) return 'weekly';
        if (_hasPrefix(heading, COMBINED_HEADINGS)) return 'combined';
        return null;
    }

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
                // Records written before the Fable row existed have no such
                // key. Normalize to null on read so the dedup comparison
                // sees "absent" rather than undefined.
                lastFablePercent: parsed.lastFablePercent === undefined ? null : parsed.lastFablePercent,
            };
            if (parsed.lastSessionActive !== undefined) result.lastSessionActive = parsed.lastSessionActive;
            if (parsed.lastWeeklyActive !== undefined) result.lastWeeklyActive = parsed.lastWeeklyActive;
            return result;
        } catch (_) {
            return null;
        }
    }

    function recordSentState({ sentAtMs, percent, resetText, windowEndsMs, sessionActive, weeklyActive, fablePercent }) {
        try {
            const storage = (typeof globalThis !== 'undefined' && globalThis.localStorage) || null;
            if (!storage) return;
            const record = {
                lastSentAtMs: sentAtMs,
                lastPercent: percent,
                lastResetText: resetText,
                lastWindowEndsMs: windowEndsMs,
                // Always written (null when the row is absent) so the dedup
                // comparison has a stable reference on both sides.
                lastFablePercent: fablePercent === undefined ? null : fablePercent,
            };
            if (sessionActive !== undefined) record.lastSessionActive = sessionActive;
            if (weeklyActive !== undefined) record.lastWeeklyActive = weeklyActive;
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
    // Absent-ness normalizer: a missing key and an explicit null must
    // compare equal, or a page with no Fable row read against a pre-Fable
    // state record would differ on every trigger and defeat the dedup.
    function _absentAsNull(v) {
        return v === undefined ? null : v;
    }

    // Floor on the send cadence while a session window is active. See
    // lib/dedup.js: the September 2026 page's absolute reset time no longer
    // ticks every minute, so without this a flat percent would go silent
    // past the 15-minute continuity gap and the Slack baseline-age gate.
    const HEARTBEAT_MS = 5 * 60 * 1000;

    function shouldSend(observation, prevState, lastObservedAgeMs, nowMs) {
        if (!prevState) return 'send';

        if (observation.sessionUsed !== prevState.lastPercent) return 'send';

        // Fable gets its own signal because its cap is tighter than the
        // session window's, so it can gain a whole point while the session
        // bar is still rounding to the same integer. The weekly aggregate
        // needs no such check: its denominator is larger than the session's,
        // so it cannot advance without the session percent advancing first.
        if (_absentAsNull(observation.fableWeeklyUsed) !== _absentAsNull(prevState.lastFablePercent)) {
            return 'send';
        }

        if (observation.resetText !== prevState.lastResetText) return 'send';

        const wasLimbo = prevState.lastSessionActive === false;
        const nowLimbo = observation.sessionActive === false;
        if (wasLimbo !== nowLimbo) return 'send';

        const wasWeeklyLimbo = prevState.lastWeeklyActive === false;
        const nowWeeklyLimbo = observation.weeklyActive === false;
        if (wasWeeklyLimbo !== nowWeeklyLimbo) return 'send';

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
        } else if (typeof nowMs === 'number' && typeof prevState.lastSentAtMs === 'number' &&
            nowMs - prevState.lastSentAtMs >= HEARTBEAT_MS) {
            return 'send';
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

    // Mirror of userscript/lib/route.js — edit both together. Accepts the
    // legacy "/settings/usage" path and the hash-routed modal form
    // ("/new#settings/usage") introduced in the June 2026 redesign.
    function isUsageRoute(pathname, hash) {
        if (pathname === USAGE_PATH) return true;
        const route = String(hash || '').replace(/^#/, '');
        return /^\/?settings\/usage(?:[/?]|$)/.test(route);
    }

    function onUsagePage() {
        return isUsageRoute(location.pathname, location.hash);
    }

    // ---------- usage-bar recognition (mirror of userscript/lib/bars.js) ----------
    //
    // Markup history (see lib/bars.js for the full rationale):
    //   - Through early July 2026 each usage bar was
    //     <div role="progressbar" aria-label="Usage" aria-valuenow="…">.
    //   - As of July 2026 the page uses a design-system Meter component:
    //     <div data-cds="Meter"><div role="meter" aria-valuenow="…"
    //     aria-labelledby="…"> — role changed to "meter", no aria-label.
    // The "Usage credits" meter also matches; section-heading anchoring in
    // extractQuota discards it because its heading matches neither section.

    const USAGE_BAR_SELECTOR =
        '[role="progressbar"][aria-label="Usage"], [role="meter"][aria-valuenow]';

    function isUsageBarTarget(role, ariaLabel) {
        if (role === 'meter') return true;
        return role === 'progressbar' && ariaLabel === 'Usage';
    }

    // ---------- DOM extraction ----------

    // For each usage bar, the most recent <h2> in document order tells us
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

    // Resolve a usage bar's accessible name. The July 2026 Meter markup
    // carries no aria-label; the name comes from aria-labelledby pointing at
    // the row-label span (possibly several ids, space-separated, per ARIA).
    // Falls back to aria-label for the legacy progressbar generation, and
    // returns null when neither resolves — callers must treat that as
    // "unknown row", never as a match.
    function resolveBarLabel(bar) {
        const ids = (bar.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean);
        if (ids.length) {
            const parts = [];
            for (const id of ids) {
                const node = document.getElementById(id);
                if (node) parts.push((node.textContent || '').trim());
            }
            const joined = parts.join(' ').trim();
            if (joined) return joined;
        }
        return bar.getAttribute('aria-label');
    }

    // The subtree that makes up one usage row: walk up from the bar while the
    // parent still holds no other usage bar and no section heading. Row text
    // (the reset hint, the limbo copy) is searched within this subtree only.
    // Through August 2026 the rows we read sat in different sections, so a
    // fixed six-level walk rarely strayed into another row; in the September
    // 2026 combined section the session and weekly rows are siblings and a
    // walk that reaches their shared container reads the *first* row's text
    // for every row. The eight-level cap bounds the climb when a bar is the
    // only one on the page.
    const ROW_ROOT_MAX_CLIMB = 8;

    function usageRowRoot(bar) {
        let node = bar;
        for (let i = 0; i < ROW_ROOT_MAX_CLIMB && node.parentElement; i++) {
            const parent = node.parentElement;
            if (parent.querySelectorAll(USAGE_BAR_SELECTOR).length > 1) break;
            if (parent.querySelector('h1, h2, h3, h4')) break;
            node = parent;
        }
        return node;
    }

    // Leaf elements (no element children) within a row, in document order.
    // Anthropic has shipped the row copy inside <p>, <span>, and <div>
    // elements at various points; the leaf restriction prevents matching a
    // container whose textContent starts with the hint but trails into
    // other copy.
    function rowLeafTexts(bar) {
        const out = [];
        for (const el of usageRowRoot(bar).querySelectorAll('*')) {
            if (el.children.length > 0) continue;
            out.push((el.textContent || '').trim());
        }
        return out;
    }

    // Locate the row's reset hint: "Resets in 19 min", "Resets Thu 11:00 PM",
    // "Resets May 1". Null when the row carries none (limbo, or a sub-row
    // whose hint is folded into other copy).
    function findRowResetText(bar) {
        for (const t of rowLeafTexts(bar)) {
            if (/^Resets\b/i.test(t)) return t;
        }
        return null;
    }

    // Detect the "no active window" limbo label on a row. Anthropic uses the
    // same copy ("Starts when a message is sent") on both the session row and
    // the weekly row when the corresponding window is not open. Scoped to
    // the row so similar marketing/help text elsewhere on the page — or the
    // sibling row's limbo copy — can't trigger a false match.
    function isLimboLabel(bar) {
        const needle = 'starts when a message is sent';
        return rowLeafTexts(bar).some(t => t.toLowerCase().includes(needle));
    }

    // ---------- reset-hint parsing (mirror of userscript/lib/resets.js) ----------
    //
    // See lib/resets.js for the full rationale. The session hint was
    // relative ("Resets in 3 hr 33 min") through August 2026 and is an
    // absolute local clock time ("Resets Thu 3:50 AM") as of September
    // 2026; the weekly hint has always been absolute ("Resets Thu 11:00 PM",
    // spelled "Thursday" since September 2026). A session reset resolves to
    // the *nearest* occurrence of that weekday/time to the observation (a
    // just-passed reset on a stale page stays a few minutes in the past,
    // which the server accepts); a weekly reset resolves to the next future
    // occurrence. Edit both copies together; test/inline-drift.test.js
    // enforces that they agree.
    const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
    const WEEKDAY_CLOCK_RE = /Resets\s+(Sun|Mon|Tue|Wed|Thu|Fri|Sat)[a-z]*\s+(\d{1,2}):(\d{2})\s*(AM|PM)/i;

    function parseWeekdayClock(text) {
        if (!text) return null;
        const m = String(text).match(WEEKDAY_CLOCK_RE);
        if (!m) return null;
        const key = m[1].slice(0, 3).toLowerCase();
        const dow = WEEKDAYS.findIndex(d => d.toLowerCase() === key);
        if (dow < 0) return null;
        let hour = parseInt(m[2], 10) % 12;
        if (m[4].toUpperCase() === 'PM') hour += 12;
        return { dow, hour, minute: parseInt(m[3], 10) };
    }

    function nearestWeekdayClockMs(clock, baseMs) {
        let best = null;
        for (let d = -3; d <= 3; d++) {
            const c = new Date(baseMs);
            c.setDate(c.getDate() + d);
            c.setHours(clock.hour, clock.minute, 0, 0);
            if (c.getDay() !== clock.dow) continue;
            const dist = Math.abs(c.getTime() - baseMs);
            if (best === null || dist < best.dist) best = { ms: c.getTime(), dist };
        }
        return best === null ? null : best.ms;
    }

    // baseMs is the wall-clock time the reset string was current — typically
    // Date.now() minus the page's "Last updated: N minutes ago" staleness, so
    // a stale page doesn't shift the computed end forward in time.
    function parseSessionEnds(text, baseMs) {
        if (!text) return null;
        const rel = String(text).match(/Resets in\s+(?:(\d+)\s*hr)?\s*(?:(\d+)\s*min)?/i);
        if (rel) {
            const hours = parseInt(rel[1] || '0', 10);
            const mins = parseInt(rel[2] || '0', 10);
            if (hours === 0 && mins === 0) return null;
            return new Date(baseMs + (hours * 60 + mins) * 60 * 1000).toISOString();
        }
        const clock = parseWeekdayClock(text);
        if (!clock) return null;
        const ms = nearestWeekdayClockMs(clock, baseMs);
        return ms === null ? null : new Date(ms).toISOString();
    }

    // Absolute clock-time hints are unaffected by page staleness, so nowMs
    // is the real wall clock (defaulting to Date.now()). "Resets May 1"
    // style hints are not parsed; null makes the server skip minting a
    // weekly window until a parseable hint arrives, and the dashboard
    // renders a [now, now+7d] hypothetical projection in the meantime.
    function parseWeeklyEnds(text, nowMs) {
        const clock = parseWeekdayClock(text);
        if (!clock) return null;
        const now = typeof nowMs === 'number' ? nowMs : Date.now();
        const target = new Date(now);
        target.setHours(clock.hour, clock.minute, 0, 0);
        for (let i = 0; i < 8; i++) {
            if (target.getDay() === clock.dow && target.getTime() > now) break;
            target.setDate(target.getDate() + 1);
        }
        return target.toISOString();
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

    // Returns { sessionUsed, weeklyUsed, sessionWindowEnds, weeklyWindowEnds,
    //           sessionActive, weeklyActive, observedAtMs, resetText,
    //           lastUpdatedAgeMs }
    // or null when neither section yields a usable bar. observedAtMs is the
    // wall-clock time the page's numbers were accurate (Date.now() minus the
    // "Last updated" staleness, or Date.now() when the indicator is missing).
    // sessionActive / weeklyActive are false only when the limbo label is
    // positively detected on the corresponding row; left undefined otherwise
    // (we never assert true). resetText is the verbatim "Resets in …" text on
    // the session row (null in limbo or when missing), used by the dedup layer
    // to spot string ticks. lastUpdatedAgeMs is the raw "Last updated"
    // staleness in ms (null when unparsable).
    function extractQuota() {
        // Anthropic moved the section headings from <h2> to <h3> as of late
        // April 2026 and started appending plan-tier badges to the heading
        // text (e.g. "Plan usage limitsMax (20x)", "Your usage limitsTeam");
        // the September 2026 combined section is back to an <h2>. We accept
        // either tag and classify the heading text by prefix (classifySection)
        // so a trailing badge or rename doesn't break extraction.
        const headings = Array.from(document.querySelectorAll('h2, h3'))
            .map(h => ({ node: h, text: (h.textContent || '').trim() }));
        const bars = document.querySelectorAll(USAGE_BAR_SELECTOR);

        const lastUpdatedAgeMs = findLastUpdatedAgeMs();
        const observedAtMs = Date.now() - (lastUpdatedAgeMs || 0);

        let sessionUsed = null, weeklyUsed = null, fableWeeklyUsed = null;
        let sessionEnds = null, weeklyEnds = null;
        let sessionActive;
        let weeklyActive;
        let sessionResetText = null;

        // First claim wins for each row; later bars with the same
        // classification are ignored.
        const claimSession = (bar, value) => {
            if (sessionUsed !== null) return;
            sessionUsed = value;
            sessionResetText = findRowResetText(bar);
            sessionEnds = parseSessionEnds(sessionResetText, observedAtMs);
            if (isLimboLabel(bar)) sessionActive = false;
        };
        const claimWeekly = (bar, value) => {
            if (weeklyUsed !== null) return;
            weeklyUsed = value;
            // Weekly hint is an absolute clock time ("Resets Thu 11:00 PM"),
            // so page staleness doesn't shift it.
            weeklyEnds = parseWeeklyEnds(findRowResetText(bar));
            if (isLimboLabel(bar)) weeklyActive = false;
        };
        const claimFable = (value) => {
            if (fableWeeklyUsed === null) fableWeeklyUsed = value;
        };

        for (const bar of bars) {
            const heading = precedingHeading(bar, headings);
            if (!heading) continue;

            const value = parseFloat(bar.getAttribute('aria-valuenow'));
            if (Number.isNaN(value)) continue;

            const section = classifySection(heading);
            if (section === 'session') {
                // Legacy two-section layout (through August 2026): the first
                // bar under the session heading is "Current session".
                claimSession(bar, value);
            } else if (section === 'weekly') {
                // Legacy weekly section: an aggregate row plus per-model
                // sub-rows. Fable is claimed by label; the aggregate stays
                // positional (first non-Fable bar in the section), so a label
                // rename can only ever cost us the fable series, never the
                // weekly line. Checking the label first also means we stay
                // correct if Anthropic ever renders Fable above "All models".
                if (isFableRowLabel(resolveBarLabel(bar))) {
                    claimFable(value);
                } else {
                    claimWeekly(bar, value);
                }
            } else if (section === 'combined') {
                // September 2026 layout: one "Your usage" section holding
                // "Current session", "This week" and "Fable this week". With
                // a single heading for three rows there is no positional
                // rule; every row is claimed by label, and unrecognised
                // labels are ignored rather than guessed.
                const row = classifyUsageRow(resolveBarLabel(bar));
                if (row === 'session') claimSession(bar, value);
                else if (row === 'weekly') claimWeekly(bar, value);
                else if (row === 'fable') claimFable(value);
            }
        }

        // The Fable row alone is not enough to call the page parsed: it is an
        // optional sub-row, so treating it as sufficient would suppress the
        // parse-error report that fires when the rows we actually depend on
        // have gone missing.
        if (sessionUsed === null && weeklyUsed === null) return null;
        return {
            sessionUsed,
            weeklyUsed,
            // Null when the row is absent — the account's plan may not show
            // it, and it did not exist at all before July 2026.
            fableWeeklyUsed,
            sessionWindowEnds: sessionEnds,
            weeklyWindowEnds: weeklyEnds,
            sessionActive,
            weeklyActive,
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
            // Match the same tag set extractQuota anchors on so a heading
            // rename or h2→h3 shuffle is visible in the fingerprint.
            const headings = Array.from(document.querySelectorAll('h2, h3'))
                .map(h => (h.textContent || '').trim().slice(0, 80))
                .filter(Boolean)
                .slice(0, 30);
            const fp = {
                pathname: location.pathname,
                heading_count: headings.length,
                heading_texts: headings,
                progressbar_count: document.querySelectorAll('[role="progressbar"]').length,
                meter_count: document.querySelectorAll('[role="meter"]').length,
                usage_bar_count: document.querySelectorAll(USAGE_BAR_SELECTOR).length,
                // Resolved accessible names of the usage bars. These are
                // Anthropic's row labels ("Current session", "Claude Code"),
                // not user content; the September 2026 break would have been
                // self-diagnosing with them in the fingerprint.
                usage_bar_labels: Array.from(document.querySelectorAll(USAGE_BAR_SELECTOR))
                    .slice(0, 12)
                    .map(bar => String(resolveBarLabel(bar) || '').slice(0, 40)),
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
        // Omitted when the row is absent, so the server records NULL rather
        // than a fabricated zero. There is no fable_window_ends: the page
        // reports the same reset time on both weekly rows.
        if (extracted.fableWeeklyUsed !== null && extracted.fableWeeklyUsed !== undefined) {
            body.fable_weekly_used = extracted.fableWeeklyUsed;
        }
        if (extracted.sessionWindowEnds) body.session_window_ends = extracted.sessionWindowEnds;
        if (extracted.weeklyWindowEnds) body.weekly_window_ends = extracted.weeklyWindowEnds;
        // Limbo signal: only emit when positively detected. We never assert
        // session_active=true / weekly_active=true — absence means "unknown".
        if (extracted.sessionActive === false) body.session_active = false;
        if (extracted.weeklyActive === false) body.weekly_active = false;
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
        const decision = shouldSend(extracted, prevState, lastObservedAgeMs, Date.now());

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
                weeklyActive: extracted.weeklyActive,
                fablePercent: extracted.fableWeeklyUsed,
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
                if (t && t.getAttribute &&
                    isUsageBarTarget(t.getAttribute('role'), t.getAttribute('aria-label'))) {
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

        const check = () => document.querySelector(USAGE_BAR_SELECTOR) !== null;
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
