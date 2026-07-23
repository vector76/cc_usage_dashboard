'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const rows = require('../lib/rows');

// The pure helpers under lib/ are the source of truth, but Tampermonkey
// loads a single file with no build step, so each helper's body is also
// inlined into claude-usage-snapshot.user.js. The docs say "edit both
// together" and nothing enforced it. This does, for the row-matching
// helper: the inlined copy is extracted from the userscript source and
// checked to agree with lib/ on every input the lib tests care about.
//
// If this fails you changed one copy and not the other.

const USERSCRIPT = fs.readFileSync(
    path.join(__dirname, '..', 'claude-usage-snapshot.user.js'),
    'utf8',
);

// Pull the inlined declarations out of the IIFE and evaluate them in
// isolation. Anchored on the exact declarations so a rename fails loudly
// here rather than silently skipping the comparison.
function loadInlinedIsFableRowLabel() {
    const prefixes = USERSCRIPT.match(/const FABLE_ROW_LABEL_PREFIXES = \[[^\]]*\];/);
    assert.ok(prefixes, 'FABLE_ROW_LABEL_PREFIXES not found in the userscript');

    const fn = USERSCRIPT.match(/function isFableRowLabel\(label\) \{[\s\S]*?\n {4}\}/);
    assert.ok(fn, 'isFableRowLabel not found in the userscript');

    // eslint-disable-next-line no-new-func
    return new Function(`${prefixes[0]}\n${fn[0]}\nreturn isFableRowLabel;`)();
}

test('inlined isFableRowLabel matches lib/rows.js', () => {
    const inlined = loadInlinedIsFableRowLabel();

    const inputs = [
        'Fable', 'fable', 'FABLE', '  Fable\n', 'Fable only', 'Fable 5',
        'FableNew', 'All models', 'Sonnet only', 'Claude Design',
        'Current session', 'Enable fable alerts', '', '   ',
        null, undefined, 77,
    ];

    for (const input of inputs) {
        assert.strictEqual(
            inlined(input),
            rows.isFableRowLabel(input),
            `inlined copy disagrees with lib/rows.js on ${JSON.stringify(input)}`,
        );
    }
});

test('inlined FABLE_ROW_LABEL_PREFIXES matches lib/rows.js', () => {
    const m = USERSCRIPT.match(/const FABLE_ROW_LABEL_PREFIXES = (\[[^\]]*\]);/);
    assert.ok(m, 'FABLE_ROW_LABEL_PREFIXES not found in the userscript');
    assert.deepStrictEqual(JSON.parse(m[1].replace(/'/g, '"')), rows.FABLE_ROW_LABEL_PREFIXES);
});

// The snapshot body is the contract with the server (internal/server/snapshot.go).
// Guard the field name specifically: a typo here fails silently — the server
// ignores unknown JSON fields, the column stays NULL, and the chart just
// never grows a second line.
test('userscript posts the fable value under the field the server reads', () => {
    assert.match(USERSCRIPT, /body\.fable_weekly_used = extracted\.fableWeeklyUsed;/);
});
