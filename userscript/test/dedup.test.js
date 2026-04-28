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
        lastUpdatedAgeMs: null,
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
        observedAtMs: 1714200060000,
        lastUpdatedAgeMs: null,
    }, overrides || {});
}

test('frozen page: same percent + same reset-text → skip every time', () => {
    const state = makeState();
    const obs = makeObservation();
    assert.strictEqual(shouldSend(obs, state), 'skip');
    assert.strictEqual(shouldSend(obs, state), 'skip');
    assert.strictEqual(shouldSend(obs, state), 'skip');
});

test('percent moves by 1 → send', () => {
    const state = makeState({ lastPercent: 42 });
    const obs = makeObservation({ sessionUsed: 43 });
    assert.strictEqual(shouldSend(obs, state), 'send');
});

test('reset-text tick: percent unchanged but reset-text decremented → send', () => {
    const state = makeState({ lastResetText: 'Resets in 3 hr 33 min' });
    const obs = makeObservation({ resetText: 'Resets in 3 hr 32 min' });
    assert.strictEqual(shouldSend(obs, state), 'send');
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
    assert.strictEqual(shouldSend(obs, state), 'send');
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
    assert.strictEqual(shouldSend(obs, state), 'send');
});

test('limbo with "Last updated" decreasing (4 min → 1 min), everything else unchanged → send', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
        lastUpdatedAgeMs: 4 * 60 * 1000,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 1 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state), 'send');
});

test('limbo with "Last updated" increasing (4 min → 5 min), everything else unchanged → skip', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
        lastUpdatedAgeMs: 4 * 60 * 1000,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 5 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state), 'skip');
});

test('limbo with "Last updated" parser returning null → skip if nothing else changed', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
        lastUpdatedAgeMs: 4 * 60 * 1000,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: null,
    });
    assert.strictEqual(shouldSend(obs, state), 'skip');
});

test('limbo with persisted age null and current age non-null, nothing else changed → skip (no info on prior side)', () => {
    const state = makeState({
        lastResetText: null,
        lastSessionActive: false,
        lastUpdatedAgeMs: null,
    });
    const obs = makeObservation({
        resetText: null,
        sessionActive: false,
        lastUpdatedAgeMs: 2 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state), 'skip');
});

test('first call ever (no persisted state) → send', () => {
    const obs = makeObservation();
    assert.strictEqual(shouldSend(obs, null), 'send');
});

test('out-of-limbo state with decreasing age does not trigger (only fires in limbo)', () => {
    const state = makeState({
        lastSessionActive: undefined,
        lastUpdatedAgeMs: 4 * 60 * 1000,
    });
    const obs = makeObservation({
        sessionActive: undefined,
        lastUpdatedAgeMs: 1 * 60 * 1000,
    });
    assert.strictEqual(shouldSend(obs, state), 'skip');
});
