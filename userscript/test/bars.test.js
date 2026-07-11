'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { USAGE_BAR_SELECTOR, isUsageBarTarget } = require('../lib/bars');

test('selector covers the legacy progressbar generation', () => {
    assert.ok(USAGE_BAR_SELECTOR.includes('[role="progressbar"][aria-label="Usage"]'));
});

test('selector covers the July 2026 meter generation', () => {
    assert.ok(USAGE_BAR_SELECTOR.includes('[role="meter"]'));
});

test('meter with no aria-label (July 2026 markup) → usage bar', () => {
    assert.strictEqual(isUsageBarTarget('meter', null), true);
});

test('meter with an unrelated aria-label (Usage credits) → still a candidate; heading anchoring excludes it downstream', () => {
    assert.strictEqual(isUsageBarTarget('meter', 'Usage credits'), true);
});

test('legacy progressbar with aria-label="Usage" → usage bar', () => {
    assert.strictEqual(isUsageBarTarget('progressbar', 'Usage'), true);
});

test('progressbar without the Usage label → not a usage bar', () => {
    assert.strictEqual(isUsageBarTarget('progressbar', null), false);
    assert.strictEqual(isUsageBarTarget('progressbar', 'Download'), false);
});

test('unrelated roles → not a usage bar', () => {
    assert.strictEqual(isUsageBarTarget('switch', 'Usage credits'), false);
    assert.strictEqual(isUsageBarTarget(null, 'Usage'), false);
    assert.strictEqual(isUsageBarTarget(undefined, undefined), false);
});
