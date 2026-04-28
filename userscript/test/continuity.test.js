'use strict';

const test = require('node:test');
const assert = require('node:assert');

const {
    decideContinuity,
    WALL_CLOCK_GAP_MS,
    WINDOW_ENDS_JUMP_MS,
} = require('../lib/continuity');

const NOW = 1714200000000;

function makeState(overrides) {
    return Object.assign({
        lastSentAtMs: NOW - 60 * 1000,
        lastPercent: 42,
        lastResetText: 'Resets in 3 hr 33 min',
        lastWindowEndsMs: NOW + 3 * 60 * 60 * 1000,
        lastSessionActive: undefined,
        lastUpdatedAgeMs: null,
    }, overrides || {});
}

function makeObservation(overrides) {
    return Object.assign({
        percent: 42,
        resetText: 'Resets in 3 hr 33 min',
        windowEndsMs: NOW + 3 * 60 * 60 * 1000,
        sessionActive: undefined,
        observedAtMs: NOW,
    }, overrides || {});
}

test('cold start (no persisted state) → false', () => {
    assert.strictEqual(decideContinuity(makeObservation(), null, NOW), false);
});

test('frozen-then-thaws: lastSentAtMs is 30 min ago → false', () => {
    const state = makeState({ lastSentAtMs: NOW - 30 * 60 * 1000 });
    assert.strictEqual(decideContinuity(makeObservation(), state, NOW), false);
});

test('within-threshold continuation → true', () => {
    const state = makeState({ lastSentAtMs: NOW - 60 * 1000 });
    assert.strictEqual(decideContinuity(makeObservation(), state, NOW), true);
});

test('percent-decrease 60 → 0 → false', () => {
    const state = makeState({ lastPercent: 60 });
    const obs = makeObservation({ percent: 0 });
    assert.strictEqual(decideContinuity(obs, state, NOW), false);
});

test('window-ends jump > 1 hr → false', () => {
    const base = NOW + 3 * 60 * 60 * 1000;
    const state = makeState({ lastWindowEndsMs: base });
    const obs = makeObservation({ windowEndsMs: base + (60 * 60 * 1000 + 60 * 1000) });
    assert.strictEqual(decideContinuity(obs, state, NOW), false);
});

test('window-ends drift within tolerance → true', () => {
    const base = NOW + 3 * 60 * 60 * 1000;
    const state = makeState({ lastWindowEndsMs: base });
    const obs = makeObservation({ windowEndsMs: base + 5 * 60 * 1000 });
    assert.strictEqual(decideContinuity(obs, state, NOW), true);
});

test('limbo→active transition: percent 0→0, window_ends shifts by 5 min → true', () => {
    const base = NOW + 3 * 60 * 60 * 1000;
    const state = makeState({
        lastPercent: 0,
        lastSessionActive: false,
        lastWindowEndsMs: base,
    });
    const obs = makeObservation({
        percent: 0,
        sessionActive: undefined,
        windowEndsMs: base + 5 * 60 * 1000,
    });
    assert.strictEqual(decideContinuity(obs, state, NOW), true);
});

test('active→limbo at non-zero usage: percent 60→0 → false (rule 3)', () => {
    const state = makeState({
        lastPercent: 60,
        lastSessionActive: undefined,
    });
    const obs = makeObservation({
        percent: 0,
        sessionActive: false,
        windowEndsMs: null,
    });
    assert.strictEqual(decideContinuity(obs, state, NOW), false);
});

test('active→limbo at zero usage: percent 0→0, no big window jump → true', () => {
    const state = makeState({
        lastPercent: 0,
        lastSessionActive: undefined,
        lastWindowEndsMs: NOW + 3 * 60 * 60 * 1000,
    });
    const obs = makeObservation({
        percent: 0,
        sessionActive: false,
        windowEndsMs: null,
    });
    assert.strictEqual(decideContinuity(obs, state, NOW), true);
});

test('F5 healthy: lastSentAtMs ~1 min ago, percent and reset-text changed → true', () => {
    const state = makeState({
        lastSentAtMs: NOW - 60 * 1000,
        lastPercent: 41,
        lastResetText: 'Resets in 3 hr 34 min',
    });
    const obs = makeObservation({
        percent: 42,
        resetText: 'Resets in 3 hr 33 min',
    });
    assert.strictEqual(decideContinuity(obs, state, NOW), true);
});

test('F5 stale: lastSentAtMs > 15 min ago → false', () => {
    const state = makeState({ lastSentAtMs: NOW - (WALL_CLOCK_GAP_MS + 60 * 1000) });
    assert.strictEqual(decideContinuity(makeObservation(), state, NOW), false);
});

test('thresholds are exported with documented values', () => {
    assert.strictEqual(WALL_CLOCK_GAP_MS, 15 * 60 * 1000);
    assert.strictEqual(WINDOW_ENDS_JUMP_MS, 60 * 60 * 1000);
});
