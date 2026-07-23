'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { isFableRowLabel } = require('../lib/rows');

test('matches the label as observed on the page', () => {
    assert.strictEqual(isFableRowLabel('Fable'), true);
});

test('case-insensitive', () => {
    assert.strictEqual(isFableRowLabel('fable'), true);
    assert.strictEqual(isFableRowLabel('FABLE'), true);
});

test('tolerates surrounding whitespace from textContent', () => {
    assert.strictEqual(isFableRowLabel('  Fable\n'), true);
});

test('prefix match absorbs version suffixes and qualifiers', () => {
    // The forms Anthropic has used on sibling rows ("Sonnet only") and the
    // shape a model-version bump would take.
    assert.strictEqual(isFableRowLabel('Fable only'), true);
    assert.strictEqual(isFableRowLabel('Fable 5'), true);
    // Design-system badge concatenated onto the label, as already seen on
    // the section headings ("Plan usage limitsMax (20x)").
    assert.strictEqual(isFableRowLabel('FableNew'), true);
});

test('the weekly aggregate row is not a fable row', () => {
    assert.strictEqual(isFableRowLabel('All models'), false);
});

test('sibling per-model rows are not fable rows', () => {
    assert.strictEqual(isFableRowLabel('Sonnet only'), false);
    assert.strictEqual(isFableRowLabel('Claude Design'), false);
});

test('the session row is not a fable row', () => {
    assert.strictEqual(isFableRowLabel('Current session'), false);
});

test('a label that merely contains "fable" does not match', () => {
    // Prefix, not substring: a future "Enable fable alerts" style row must
    // not be mistaken for the meter row.
    assert.strictEqual(isFableRowLabel('Enable fable alerts'), false);
});

test('missing or non-string labels are not a match', () => {
    assert.strictEqual(isFableRowLabel(null), false);
    assert.strictEqual(isFableRowLabel(undefined), false);
    assert.strictEqual(isFableRowLabel(''), false);
    assert.strictEqual(isFableRowLabel('   '), false);
    assert.strictEqual(isFableRowLabel(77), false);
});
