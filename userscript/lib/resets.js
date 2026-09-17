'use strict';

// Pure parsers for the "Resets …" hint on a usage row.
//
// Hint history:
//   - Session row, through August 2026: relative -- "Resets in 3 hr 33 min",
//     "Resets in 19 min", "Resets in 5 hr". Resolved as baseMs + delta,
//     where baseMs is the wall-clock time the text was current (Date.now()
//     minus the page's "Last updated" staleness).
//   - Weekly row, all revisions: absolute local clock time with a weekday --
//     "Resets Thu 11:00 PM". September 2026 spells the weekday out
//     ("Resets Thursday 11:00 PM").
//   - Session row, September 2026: the same absolute form as the weekly
//     row ("Resets Thu 3:50 AM").
//
// The two rows resolve an absolute time differently on purpose. A weekly
// reset is up to seven days out, so "next future occurrence" is the only
// sensible reading. A session window is at most five hours, so the reset
// is always within a few hours either side of the observation: we take the
// *nearest* occurrence of that weekday and clock time to baseMs. On a
// stale page whose reset has just passed that lands a few minutes in the
// past, which is the truth and which the server accepts (it tolerates an
// end up to an hour back); "next future" would instead jump a whole week
// ahead and be rejected as too far in the future.
//
// Clock times are in the browser's local timezone; Date's local setters do
// the conversion, DST included.
//
// This file is the single source of truth. Its body is also inlined into
// ../claude-usage-snapshot.user.js so Tampermonkey runs without a build
// step; tests load it via require() from here.

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const WEEKDAY_CLOCK_RE = /Resets\s+(Sun|Mon|Tue|Wed|Thu|Fri|Sat)[a-z]*\s+(\d{1,2}):(\d{2})\s*(AM|PM)/i;

// parseWeekdayClock reads "Resets <weekday> <h>:<mm> <AM|PM>" into
// { dow (0 = Sunday), hour (0-23), minute }, or null.
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

// nearestWeekdayClockMs returns the epoch ms of the occurrence of the given
// weekday and local clock time closest to baseMs, searching three days
// either side so every weekday is reachable exactly once.
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

// parseSessionEnds accepts both the relative and the absolute session
// hint. baseMs is the wall-clock time the text was current. Returns a UTC
// ISO string or null.
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

// parseWeeklyEnds resolves the weekly hint to the next occurrence of that
// weekday and local clock time strictly after nowMs. Absolute clock times
// are unaffected by page staleness, so nowMs is the real wall clock
// (defaulting to Date.now()). Formats like "Resets May 1", which Anthropic
// uses when the reset is far enough out to show a date, are not parsed;
// null makes the server skip minting a weekly window until a parseable
// hint arrives.
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

if (typeof module !== 'undefined') {
    module.exports = {
        WEEKDAYS,
        WEEKDAY_CLOCK_RE,
        parseWeekdayClock,
        nearestWeekdayClockMs,
        parseSessionEnds,
        parseWeeklyEnds,
    };
}
