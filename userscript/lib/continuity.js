'use strict';

// Pure decision function for the continuous_with_prev flag carried on
// every outgoing snapshot. Given the latest DOM-extracted observation
// and the last successfully sent state, returns a boolean: true when
// the new datapoint can be linearly chained off the previous one,
// false when downstream consumers should treat it as the start of a
// fresh segment (cold start, long gap, percent reset, or window-ends
// jump).
//
// No DOM or network access. This file is the single source of truth;
// its body is also inlined into ../claude-usage-snapshot.user.js so
// Tampermonkey runs without a build step. Tests load it via require().

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

if (typeof module !== 'undefined') {
    module.exports = {
        decideContinuity,
        WALL_CLOCK_GAP_MS,
        WINDOW_ENDS_JUMP_MS,
    };
}
