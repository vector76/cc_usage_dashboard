'use strict';

// Pure decision function for the userscript's freshness-driven dedup.
// Given the latest DOM-extracted observation and the last successfully
// sent state, returns 'send' when at least one meaningful-change signal
// has fired since the previous send, or 'skip' otherwise.
//
// No DOM or network access — callers are responsible for extracting the
// observation and persisting the state.
//
// This file is the single source of truth. Its body is also inlined
// into ../claude-usage-snapshot.user.js so Tampermonkey runs without
// a build step; tests load it via require() from here.

function shouldSend(observation, prevState) {
    if (!prevState) return 'send';

    if (observation.sessionUsed !== prevState.lastPercent) return 'send';

    if (observation.resetText !== prevState.lastResetText) return 'send';

    const wasLimbo = prevState.lastSessionActive === false;
    const nowLimbo = observation.sessionActive === false;
    if (wasLimbo !== nowLimbo) return 'send';

    if (nowLimbo) {
        // While in limbo the visible numbers don't move, so a strict
        // *decrease* in "Last updated" age is our only signal that a
        // fresh poll landed. Null on either side is "no information"
        // and must not fire. We deliberately do NOT fire on the age
        // *incrementing* — that advances on pure wall-clock time and
        // would re-introduce the spam dedup is meant to prevent.
        const cur = observation.lastUpdatedAgeMs;
        const prev = prevState.lastUpdatedAgeMs;
        if (cur != null && prev != null && cur < prev) return 'send';
    }

    return 'skip';
}

if (typeof module !== 'undefined') {
    module.exports = { shouldSend };
}
