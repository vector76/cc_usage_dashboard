'use strict';

const test = require('node:test');
const assert = require('node:assert');

const {
    presentFamilies,
    VOLUME_FAMILY_ORDER,
} = require('../../internal/dashboard/static/grouping');

function bucket(byFamily, costUsd) {
    const total = costUsd != null
        ? costUsd
        : Object.values(byFamily || {}).reduce((a, b) => a + b, 0);
    const b = { bucket_start: '2026-09-01T00:00:00Z', cost_usd: total };
    if (byFamily) b.by_family = byFamily;
    return b;
}

// The legend is a key to the bars actually on screen, so it lists exactly the
// families with dollars in the session or weekly payload — nothing for an
// account that never touches sonnet, and only one of fable / mythos.
test('presentFamilies lists only families with nonzero cost', () => {
    const session = { volume: [bucket({ opus: 1.5 }), bucket({ opus: 0.2, haiku: 0.01 })] };
    const weekly = { volume: [bucket({ fable: 3 })] };
    assert.deepStrictEqual(presentFamilies([session, weekly]), ['opus', 'fable', 'haiku']);
});

test('presentFamilies unions session and weekly windows', () => {
    const session = { volume: [bucket({ sonnet: 0.5 })] };
    const weekly = { volume: [bucket({ mythos: 2 })] };
    assert.deepStrictEqual(presentFamilies([session, weekly]), ['sonnet', 'mythos']);
});

test('presentFamilies keeps the fixed stack order regardless of input order', () => {
    const w = { volume: [bucket({ other: 1 }), bucket({ haiku: 1 }), bucket({ opus: 1 })] };
    assert.deepStrictEqual(presentFamilies([w]), ['opus', 'haiku', 'other']);
    for (const fam of presentFamilies([w])) assert.ok(VOLUME_FAMILY_ORDER.includes(fam));
});

test('presentFamilies ignores zero, negative, and non-numeric entries', () => {
    const w = { volume: [bucket({ opus: 0, sonnet: -1, fable: null, haiku: 'x', mythos: 0.5 })] };
    assert.deepStrictEqual(presentFamilies([w]), ['mythos']);
});

test('presentFamilies drops family names the legend does not know', () => {
    const w = { volume: [bucket({ opus: 1, gemini: 5 })] };
    assert.deepStrictEqual(presentFamilies([w]), ['opus']);
});

// A bucket carrying a total but no by_family map renders as a single "other"
// segment (older/degraded payloads); the legend has to explain that segment.
test('presentFamilies counts a total without by_family as other', () => {
    const w = { volume: [bucket(null, 0.75)] };
    assert.deepStrictEqual(presentFamilies([w]), ['other']);
    const empty = { volume: [bucket({}, 0.75)] };
    assert.deepStrictEqual(presentFamilies([empty]), ['other']);
    const zero = { volume: [bucket(null, 0)] };
    assert.deepStrictEqual(presentFamilies([zero]), []);
});

test('presentFamilies is empty for missing, null, or volume-less windows', () => {
    assert.deepStrictEqual(presentFamilies([]), []);
    assert.deepStrictEqual(presentFamilies([null, undefined]), []);
    assert.deepStrictEqual(presentFamilies([{}, { volume: null }, { volume: [] }]), []);
    assert.deepStrictEqual(presentFamilies(null), []);
});
