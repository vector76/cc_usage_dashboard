'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { shouldSend } = require('../lib/dedup');

function makeState(overrides) {
    return Object.assign({
        lastSentAtMs: 1714200000000,
        lastPercent: 42,
        lastResetText: 'Resets in 3 hr 33 min',
        lastWindowEndsMs: 1714212600000,
        lastSessionActive: undefined,
        lastWeeklyActive: undefined,
    }, overrides || {});
}

function makeObservation(overrides) {
    return Object.assign({
        sessionUsed: 42,
        weeklyUsed: 10,
        resetText: 'Resets in 3 hr 33 min',
        sessionWindowEnds: null,
        weeklyWindowEnds: null,
        sessionActive: undefined,
        weeklyActive: undefined,
        observedAtMs: 1714200060000,
        lastUpdatedAgeMs: null,
    }, overrides || {});
}

test('frozen page: same percent + same reset-text → skip every time', () => {
    const state = makeState();
    const obs = makeObservation();
    assert.strictEqual(shouldSend(obs, state, null), 'skip');
    assert.strictEqual(shouldSend(obs, state, null), 'skip');
    assert.strictEqual(shouldSend(obs, state, null), 'skip');
});

test('percent moves by 1 → send', () => {
    const state = makeState({ lastPercent: 42 });
    const obs = makeObservation({ sessionUsed: 43 });
    assert.strictEqual(shouldSend(obs, state, null), 'send');
});

test('reset-text tick: percent unchanged but reset-text decremented → send', () => {
    const state = makeState({ lastResetText: 'Resets in 3 hr 33 min' });
    const obs = makeObservation({ resetText: 'Resets in 3 hr 32 min' });
    assert.strictEqual(shouldSend(obs, state, null), 'send');
});

test('limbo entry: row text changes from "Resets in …" to "Starts when a message is sent" → send', () => {
    const state = makeState({
        lastResetText: 'Resets in 2 hr 31 min',
        lastSessionActive: undefined,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
    });
    assert.strictEqual(shouldSend(obs, state, null), 'send');
});

test('limbo exit: from limbo back to "Resets in …" → send', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
    });
    const obs = makeObservation({
        resetText: 'Resets in 2 hr 30 min',
        sessionActive: undefined,
    });
    assert.strictEqual(shouldSend(obs, state, null), 'send');
});

test('limbo with rolling-age decreasing (4 min → 1 min), everything else unchanged → send', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 1 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state, 4 * 60 * 1000), 'send');
});

test('limbo with rolling-age increasing (4 min → 5 min), everything else unchanged → skip', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 5 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state, 4 * 60 * 1000), 'skip');
});

test('limbo with current age null (parser unparsable) → skip if nothing else changed', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: null,
    });
    assert.strictEqual(shouldSend(obs, state, 4 * 60 * 1000), 'skip');
});

test('limbo with rolling age null (no prior observation yet) → skip', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 2 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state, null), 'skip');
});

test('weekly limbo entry: weeklyActive flips undefined → false → send', () => {
    const state = makeState({ lastWeeklyActive: undefined });
    const obs = makeObservation({ weeklyActive: false });
    assert.strictEqual(shouldSend(obs, state, null), 'send');
});

test('weekly limbo exit: weeklyActive flips false → undefined → send', () => {
    const state = makeState({ lastWeeklyActive: false });
    const obs = makeObservation({ weeklyActive: undefined });
    assert.strictEqual(shouldSend(obs, state, null), 'send');
});

test('weekly limbo steady state: both sides false → skip', () => {
    const state = makeState({ lastWeeklyActive: false });
    const obs = makeObservation({ weeklyActive: false });
    assert.strictEqual(shouldSend(obs, state, null), 'skip');
});

test('first call ever (no persisted state) → send', () => {
    const obs = makeObservation();
    assert.strictEqual(shouldSend(obs, null, null), 'send');
});

test('out-of-limbo state with decreasing rolling age does not trigger (only fires in limbo)', () => {
    const state = makeState({
        lastSessionActive: undefined,
    });
    const obs = makeObservation({
        sessionActive: undefined,
        lastUpdatedAgeMs: 1 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state, 4 * 60 * 1000), 'skip');
});

// Regression: the original implementation compared the current age
// against prevState.lastUpdatedAgeMs, which only updated on send.
// After the first limbo send while the page showed "just now" the
// persisted age sat at 0 forever, so 0 < 0 was false on every
// subsequent read and the userscript went silent for hours. The
// rolling reference is what we actually need.
test('regression: limbo, rolling 4 min → cur 0 fires even when persisted-age is irrelevant', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 0,
    });
    assert.strictEqual(shouldSend(obs, state, 4 * 60 * 1000), 'send');
});

test('regression: limbo with rolling 0 and cur 0 (steady-state "just now") → skip', () => {
    // Once we send while the page shows "just now", the rolling
    // counter is 0 and the next read also shows "just now" (claude.ai
    // has not re-polled yet). We must NOT fire — the trigger is a
    // *decrease*, not an equality.
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 0,
    });
    assert.strictEqual(shouldSend(obs, state, 0), 'skip');
});
