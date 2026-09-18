'use strict';

const test = require('node:test');
const assert = require('node:assert');

const {
    MIN_PCT_FOR_ESTIMATE,
    extrapolateAllowance,
    slackReleaseState,
    fmtAge,
} = require('../../internal/dashboard/static/summary');

// --- extrapolateAllowance -------------------------------------------------

// The estimate answers "what would 100% of this window's allowance have
// cost?", read off the one measurement we have: dollars spent while the
// percent gauge moved by a known amount.
test('extrapolates the full allowance from a partial consumption', () => {
    // $20 bought 50% of the window, so 100% is worth $40.
    assert.strictEqual(extrapolateAllowance(20, 50), 40);
    // 25% consumed for $12.50 → $50 for the whole window.
    assert.strictEqual(extrapolateAllowance(12.5, 25), 50);
});

// A period long enough to span several windows reports over 100% consumed.
// That is more evidence, not less, and the same division holds.
test('handles percentages above 100 from multi-window periods', () => {
    // 30 days of session windows: 400% consumed for $160 → $40 per window.
    assert.strictEqual(extrapolateAllowance(160, 400), 40);
});

test('MIN_PCT_FOR_ESTIMATE is the documented 10% floor', () => {
    assert.strictEqual(MIN_PCT_FOR_ESTIMATE, 10);
});

// Below the floor, rounding and snapshot granularity dominate: a gauge that
// reads 3% could be anywhere in 2.5-3.5%, a ±17% swing on the estimate.
test('returns null at or below the 10% floor', () => {
    assert.strictEqual(extrapolateAllowance(5, 10), null);
    assert.strictEqual(extrapolateAllowance(5, 9.9), null);
    assert.strictEqual(extrapolateAllowance(5, 0), null);
    // Just above the floor is enough.
    assert.strictEqual(extrapolateAllowance(5, 10.0001) > 0, true);
});

// Percent comes from the userscript's scrape of Anthropic's gauges while
// dollars come from usage_events. When only the former is flowing the
// division would confidently report $0.00, so no-dollars is "no estimate".
test('returns null when no dollars were recorded', () => {
    assert.strictEqual(extrapolateAllowance(0, 50), null);
    assert.strictEqual(extrapolateAllowance(-1, 50), null);
});

test('returns null for missing or non-finite inputs', () => {
    assert.strictEqual(extrapolateAllowance(20, null), null);
    assert.strictEqual(extrapolateAllowance(20, undefined), null);
    assert.strictEqual(extrapolateAllowance(null, 50), null);
    assert.strictEqual(extrapolateAllowance(undefined, 50), null);
    assert.strictEqual(extrapolateAllowance(20, NaN), null);
    assert.strictEqual(extrapolateAllowance(NaN, 50), null);
    assert.strictEqual(extrapolateAllowance(20, Infinity), null);
    assert.strictEqual(extrapolateAllowance(Infinity, 50), null);
    assert.strictEqual(extrapolateAllowance(20, '50'), null);
    assert.strictEqual(extrapolateAllowance('20', 50), null);
});

// --- slackReleaseState ----------------------------------------------------

test('reports the gate verdict when the collector is running', () => {
    assert.strictEqual(slackReleaseState({ slack_release_recommended: true }), 'yes');
    assert.strictEqual(slackReleaseState({ slack_release_recommended: false }), 'no');
});

// Pausing stops the observations the headroom gates evaluate, so their
// verdict is about a stale world. "paused" says the question is unanswerable
// right now; "no" would read as a decision that slack is unavailable.
test('paused outranks the gate verdict in both directions', () => {
    assert.strictEqual(slackReleaseState({ paused: true, slack_release_recommended: true }), 'paused');
    assert.strictEqual(slackReleaseState({ paused: true, slack_release_recommended: false }), 'paused');
    assert.strictEqual(slackReleaseState({ paused: true, slack_release_recommended: null }), 'paused');
});

test('unknown covers a null verdict and a missing state payload', () => {
    assert.strictEqual(slackReleaseState({ slack_release_recommended: null }), 'unknown');
    assert.strictEqual(slackReleaseState({}), 'unknown');
    assert.strictEqual(slackReleaseState(null), 'unknown');
    assert.strictEqual(slackReleaseState(undefined), 'unknown');
});

// --- fmtAge ---------------------------------------------------------------

test('renders an age in its two coarsest informative units', () => {
    assert.strictEqual(fmtAge(0), '0 s');
    assert.strictEqual(fmtAge(47), '47 s');
    assert.strictEqual(fmtAge(59.4), '59 s');
    assert.strictEqual(fmtAge(60), '1 m 0 s');
    assert.strictEqual(fmtAge(192), '3 m 12 s');
    assert.strictEqual(fmtAge(3600), '1 h 0 m');
    assert.strictEqual(fmtAge(7500), '2 h 5 m');
    assert.strictEqual(fmtAge(86400), '1 d 0 h');
    assert.strictEqual(fmtAge(273600), '3 d 4 h');
});

// Rounding happens before bucketing, so 59.6s reads "1 m 0 s" rather than
// the contradictory "60 s".
test('rounds to whole seconds before choosing units', () => {
    assert.strictEqual(fmtAge(59.6), '1 m 0 s');
});

// Server and client clocks can disagree by a second or two; a negative age
// is clock skew, not time travel.
test('clamps a negative age to zero', () => {
    assert.strictEqual(fmtAge(-5), '0 s');
});
