'use strict';

// Pure decision function for the userscript's freshness-driven dedup.
// Given the latest DOM-extracted observation, the last successfully-sent
// state, and the most recently *observed* "Last updated" age (rolling,
// in-memory, updated on every DOM read whether or not we sent), returns
// 'send' when at least one meaningful-change signal has fired since the
// previous send, or 'skip' otherwise.
//
// The rolling lastObservedAgeMs is intentionally distinct from anything
// in prevState. In limbo we want to fire when the displayed staleness
// counter rolls back, which can happen between two successive
// observations even when no send took place. Comparing against the
// persisted last-sent age would self-trap: once a send lands while the
// page shows "just now" the persisted age sits at 0 forever, and no
// future observation can be strictly less.
//
// No DOM or network access — callers extract the observation, persist
// the state, and track the rolling observation history.
//
// This file is the single source of truth. Its body is also inlined
// into ../claude-usage-snapshot.user.js so Tampermonkey runs without
// a build step; tests load it via require() from here.

// Normalizes absent-ness so a missing key and an explicit null compare
// equal. Without this, a page with no Fable row (observation → null) read
// against a state record written before the field existed (undefined) would
// differ on every single trigger and defeat the dedup entirely.
function _absentAsNull(v) {
    return v === undefined ? null : v;
}

function shouldSend(observation, prevState, lastObservedAgeMs) {
    if (!prevState) return 'send';

    if (observation.sessionUsed !== prevState.lastPercent) return 'send';

    // The Fable weekly sub-row gets its own signal; the weekly aggregate
    // deliberately does not. The aggregate's denominator is larger than the
    // session window's, so it cannot advance a whole point without the
    // session percent advancing first — the session check already covers
    // it. Fable inverts that: its cap is visibly tighter (77% used against
    // the aggregate's 44% on the same page), so it gains points faster than
    // the session bar and can tick while the session percent is still
    // rounding to the same integer. Without this check those observations
    // would be dropped at the gate and never reach the chart.
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
    }

    return 'skip';
}

if (typeof module !== 'undefined') {
    module.exports = { shouldSend };
}
