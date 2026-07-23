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

// ---------- quota-reset splitting (rule 2) ----------

test('a weekly reset splits the polyline even when the continuity flag is true', () => {
    // The exact shape observed in production: Anthropic resets the weekly
    // quota while the user is idle, so the session bar sits at 0 and never
    // "decreases" — decideContinuity sees 0 < 0 == false and reports
    // continuity. Only the plotted percent reveals the reset.
    const points = [
        { continuous_with_prev: false, percent_used: 30, observed_at: 't0' },
        { continuous_with_prev: true,  percent_used: 39, observed_at: 't1' },
        { continuous_with_prev: true,  percent_used: 43, observed_at: 't2' },
        { continuous_with_prev: true,  percent_used: 0,  observed_at: 't3' },
        { continuous_with_prev: true,  percent_used: 4,  observed_at: 't4' },
    ];

    const groups = groupPolylines(points);
    assert.strictEqual(groups.length, 2, 'the reset must break the line');
    assert.deepStrictEqual(groups[0].map(p => p.observed_at), ['t0', 't1', 't2']);
    assert.deepStrictEqual(groups[1].map(p => p.observed_at), ['t3', 't4']);
});

test('a monotonically rising series stays in one polyline', () => {
    // Guard against over-splitting: consumption within a window only ever
    // grows, and every one of those points must stay connected.
    const points = [0, 1, 1, 7, 7, 12, 30, 44].map((v, i) => ({
        continuous_with_prev: i > 0, percent_used: v, observed_at: 't' + i,
    }));

    const groups = groupPolylines(points);
    assert.strictEqual(groups.length, 1);
    assert.strictEqual(groups[0].length, points.length);
});

test('equal consecutive percentages do not split (plateau, not reset)', () => {
    // The rule is a *strict* decrease. Plateaus are the common case —
    // splitting on them would shatter every flat stretch into dots.
    const points = [
        { continuous_with_prev: false, percent_used: 44, observed_at: 't0' },
        { continuous_with_prev: true,  percent_used: 44, observed_at: 't1' },
        { continuous_with_prev: true,  percent_used: 44, observed_at: 't2' },
    ];

    assert.strictEqual(groupPolylines(points).length, 1);
});

test('only the decrease splits; the recovery point continues the new group', () => {
    // 40 → 0 → 41. The drop breaks the line, but the rebound is an
    // *increase* and so chains onto the post-reset segment. The rule is
    // deliberately one-directional: a rise, however steep, is consumption
    // and stays connected.
    const points = [
        { continuous_with_prev: false, percent_used: 40, observed_at: 't0' },
        { continuous_with_prev: true,  percent_used: 0,  observed_at: 't1' },
        { continuous_with_prev: true,  percent_used: 41, observed_at: 't2' },
    ];

    const groups = groupPolylines(points);
    assert.strictEqual(groups.length, 2);
    assert.deepStrictEqual(groups.map(g => g.map(p => p.observed_at)),
        [['t0'], ['t1', 't2']]);
});

test('missing or non-numeric percent_used never fabricates a break', () => {
    // Absent data is "no information" — the same stance the continuity
    // flag takes. The pre-existing fixtures in this file carry no
    // percent_used at all and must keep grouping on the flag alone.
    const points = [
        { continuous_with_prev: false, observed_at: 't0' },
        { continuous_with_prev: true,  observed_at: 't1' },
        { continuous_with_prev: true,  percent_used: null, observed_at: 't2' },
        { continuous_with_prev: true,  percent_used: 5, observed_at: 't3' },
        { continuous_with_prev: true,  percent_used: 6, observed_at: 't4' },
    ];

    const groups = groupPolylines(points);
    assert.strictEqual(groups.length, 1);
    assert.strictEqual(groups[0].length, 5);
});

test('both rules can fire on the same point without producing an empty group', () => {
    const points = [
        { continuous_with_prev: false, percent_used: 40, observed_at: 't0' },
        { continuous_with_prev: false, percent_used: 0,  observed_at: 't1' },
    ];

    const groups = groupPolylines(points);
    assert.strictEqual(groups.length, 2);
    assert.deepStrictEqual(groups.map(g => g.length), [1, 1]);
});
