'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const {
    VOLUME_FAMILY_ORDER,
    VOLUME_FAMILY_COLORS,
} = require('../../internal/dashboard/static/grouping');

const STATIC_DIR = path.join(__dirname, '..', '..', 'internal', 'dashboard', 'static');

// Every family the stack/legend iterates must have a colour. Adding a family to
// the order and forgetting the colour renders it as `undefined`, which browsers
// silently drop — a segment vanishes from a bar with no error anywhere.
test('every ordered family has a colour', () => {
    for (const fam of VOLUME_FAMILY_ORDER) {
        assert.ok(VOLUME_FAMILY_COLORS[fam], `no colour for family ${fam}`);
    }
    assert.deepStrictEqual(
        Object.keys(VOLUME_FAMILY_COLORS).sort(),
        [...VOLUME_FAMILY_ORDER].sort(),
        'colour map and order list disagree on the family set',
    );
});

// The families must match what the Go classifier can actually emit. Extracted
// from the switch in ModelFamily so adding a family on one side without the
// other fails here rather than showing up as an uncoloured segment.
test('families match the Go ModelFamily classifier', () => {
    const src = fs.readFileSync(
        path.join(__dirname, '..', '..', 'internal', 'consumption', 'breakdown.go'),
        'utf8',
    );
    const fn = src.match(/func ModelFamily\(model string\) string \{[\s\S]*?\n\}/);
    assert.ok(fn, 'ModelFamily not found in internal/consumption/breakdown.go');

    const returned = [...fn[0].matchAll(/return "([a-z]+)"/g)].map((m) => m[1]);
    assert.deepStrictEqual(
        [...new Set(returned)].sort(),
        [...VOLUME_FAMILY_ORDER].sort(),
        'ModelFamily returns a family set the client does not know about',
    );
});

// Both pages load the constants from grouping.js. A page that redeclares them
// locally would shadow the shared copy and drift from it silently.
test('no page redeclares the family constants', () => {
    for (const page of ['index.html', 'report.html']) {
        const html = fs.readFileSync(path.join(STATIC_DIR, page), 'utf8');
        assert.ok(
            !/const VOLUME_FAMILY_(ORDER|COLORS)\s*=/.test(html),
            `${page} redeclares a family constant instead of using grouping.js`,
        );
        assert.match(html, /grouping\.js/, `${page} does not load grouping.js`);
    }
});
