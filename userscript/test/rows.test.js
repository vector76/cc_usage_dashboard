'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { isFableRowLabel, classifyUsageRow, classifySection } = require('../lib/rows');

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
    // September 2026 form.
    assert.strictEqual(isFableRowLabel('Fable this week'), true);
});

test('the weekly aggregate row is not a fable row', () => {
    assert.strictEqual(isFableRowLabel('All models'), false);
    assert.strictEqual(isFableRowLabel('This week'), false);
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

// ---------- September 2026 layout: one section, rows told apart by label ----------

test('classifyUsageRow: labels as observed on the September 2026 page', () => {
    assert.strictEqual(classifyUsageRow('Current session'), 'session');
    assert.strictEqual(classifyUsageRow('This week'), 'weekly');
    assert.strictEqual(classifyUsageRow('Fable this week'), 'fable');
});

test('classifyUsageRow: the July 2026 weekly aggregate label still resolves', () => {
    assert.strictEqual(classifyUsageRow('All models'), 'weekly');
    assert.strictEqual(classifyUsageRow('Fable'), 'fable');
});

test('classifyUsageRow: fable wins over weekly even though its label ends in "this week"', () => {
    assert.strictEqual(classifyUsageRow('Fable this week'), 'fable');
    assert.strictEqual(classifyUsageRow('fable THIS WEEK'), 'fable');
});

test('classifyUsageRow: case-folded prefix match with whitespace and badges', () => {
    assert.strictEqual(classifyUsageRow('  current session\n'), 'session');
    assert.strictEqual(classifyUsageRow('This weekMax (20x)'), 'weekly');
    assert.strictEqual(classifyUsageRow('Current session (5 hours)'), 'session');
});

test('classifyUsageRow: per-product and credit meters are not usage rows', () => {
    for (const label of ['Claude Code', 'Chats', 'Cowork', 'Other', 'Usage credits', 'Sonnet only', 'Claude Design']) {
        assert.strictEqual(classifyUsageRow(label), null, label);
    }
});

test('classifyUsageRow: missing or non-string labels are unknown', () => {
    assert.strictEqual(classifyUsageRow(null), null);
    assert.strictEqual(classifyUsageRow(undefined), null);
    assert.strictEqual(classifyUsageRow(''), null);
    assert.strictEqual(classifyUsageRow('   '), null);
    assert.strictEqual(classifyUsageRow(42), null);
});

test('classifySection: legacy session headings, with and without plan badges', () => {
    assert.strictEqual(classifySection('Plan usage limits'), 'session');
    assert.strictEqual(classifySection('Plan usage limitsMax (20x)'), 'session');
    assert.strictEqual(classifySection('Your usage limits'), 'session');
    assert.strictEqual(classifySection('Your usage limitsTeam'), 'session');
});

test('classifySection: legacy weekly heading', () => {
    assert.strictEqual(classifySection('Weekly limits'), 'weekly');
    assert.strictEqual(classifySection('Weekly limitsMax (20x)'), 'weekly');
});

test('classifySection: the September 2026 combined section', () => {
    assert.strictEqual(classifySection('Your usage'), 'combined');
    assert.strictEqual(classifySection('Your usageMax (20x)'), 'combined');
    assert.strictEqual(classifySection('  Your usage\n'), 'combined');
});

test('classifySection: "Your usage limits" is a session section, not the combined one', () => {
    // "Your usage" is a prefix of "Your usage limits"; the more specific
    // legacy form must win or every legacy bar would be classified by a
    // label we never resolved.
    assert.strictEqual(classifySection('Your usage limits'), 'session');
});

test('classifySection: sections we must ignore', () => {
    for (const h of ['Usage', 'Usage credits', 'Monthly spend limit',
        'This week’s usage by product', 'Additional features', '', null, undefined]) {
        assert.strictEqual(classifySection(h), null, String(h));
    }
});
