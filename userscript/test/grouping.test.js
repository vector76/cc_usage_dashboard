'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { groupPolylines } = require('../../internal/dashboard/static/grouping');

test('mixed continuity flags split into the expected polylines', () => {
    const points = [
        { continuous_with_prev: false, observed_at: 't0' },
        { continuous_with_prev: true,  observed_at: 't1' },
        { continuous_with_prev: true,  observed_at: 't2' },
        { continuous_with_prev: false, observed_at: 't3' },
        { continuous_with_prev: true,  observed_at: 't4' },
    ];

    const groups = groupPolylines(points);
    assert.strictEqual(groups.length, 2);
    assert.strictEqual(groups[0].length, 3);
    assert.strictEqual(groups[1].length, 2);
    assert.deepStrictEqual(
        groups[0].map(p => p.observed_at),
        ['t0', 't1', 't2'],
    );
    assert.deepStrictEqual(
        groups[1].map(p => p.observed_at),
        ['t3', 't4'],
    );
});

test('continuations stay in one polyline regardless of window_ends drift', () => {
    // Synthetic 30-min drift in window_ends across a run of all-true
    // continuations — the old tolerance-based grouper would have split
    // here, but the continuity-flag grouper ignores window_ends entirely
    // and keeps them in a single polyline.
    const points = [];
    for (let i = 0; i < 6; i++) {
        points.push({
            continuous_with_prev: true,
            window_ends: new Date(1714200000000 + i * 30 * 60 * 1000).toISOString(),
        });
    }

    const groups = groupPolylines(points);
    assert.strictEqual(groups.length, 1);
    assert.strictEqual(groups[0].length, 6);
});

test('a single point yields a single one-element group', () => {
    const groups = groupPolylines([{ continuous_with_prev: true, observed_at: 't0' }]);
    assert.strictEqual(groups.length, 1);
    assert.strictEqual(groups[0].length, 1);
});

test('empty input yields no groups', () => {
    assert.deepStrictEqual(groupPolylines([]), []);
});

test('first point is a polyline start even when its flag is true', () => {
    // The very first observation has no predecessor in the rendered
    // series, so its continuous_with_prev value is irrelevant — it
    // always starts a new polyline. The fixture above relies on this
    // implicitly for the second test case; pin it down explicitly here.
    const groups = groupPolylines([
        { continuous_with_prev: true, observed_at: 't0' },
        { continuous_with_prev: true, observed_at: 't1' },
    ]);
    assert.strictEqual(groups.length, 1);
    assert.strictEqual(groups[0].length, 2);
});
