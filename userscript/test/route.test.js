'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { isUsageRoute } = require('../lib/route');

test('legacy full-path route still matches', () => {
    assert.strictEqual(isUsageRoute('/settings/usage', ''), true);
});

test('hash-routed modal over a chat page matches (June 2026 redesign)', () => {
    assert.strictEqual(isUsageRoute('/new', '#settings/usage'), true);
    assert.strictEqual(isUsageRoute('/chat/abc-123', '#settings/usage'), true);
});

test('hash with a leading slash matches', () => {
    assert.strictEqual(isUsageRoute('/new', '#/settings/usage'), true);
});

test('trailing sub-path or query is tolerated', () => {
    assert.strictEqual(isUsageRoute('/new', '#settings/usage/'), true);
    assert.strictEqual(isUsageRoute('/new', '#settings/usage?ref=x'), true);
});

test('a different settings tab does not match', () => {
    assert.strictEqual(isUsageRoute('/new', '#settings/account'), false);
    assert.strictEqual(isUsageRoute('/new', '#settings/billing'), false);
});

test('no settings route at all does not match', () => {
    assert.strictEqual(isUsageRoute('/new', ''), false);
    assert.strictEqual(isUsageRoute('/chat/abc-123', ''), false);
});

test('a sibling route sharing the "usage" prefix does not match', () => {
    assert.strictEqual(isUsageRoute('/new', '#settings/usage-summary'), false);
    assert.strictEqual(isUsageRoute('/settings/usagex', ''), false);
});

test('null/undefined hash is handled without throwing', () => {
    assert.strictEqual(isUsageRoute('/new', null), false);
    assert.strictEqual(isUsageRoute('/new', undefined), false);
});
