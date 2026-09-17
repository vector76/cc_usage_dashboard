'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const rows = require('../lib/rows');
const resets = require('../lib/resets');
const dedup = require('../lib/dedup');

// The pure helpers under lib/ are the source of truth, but Tampermonkey
// loads a single file with no build step, so each helper's body is also
// inlined into claude-usage-snapshot.user.js. The docs say "edit both
// together" and nothing enforced it. This does: each inlined copy is
// extracted from the userscript source and checked to agree with lib/ on
// every input the lib tests care about.
//
// If this fails you changed one copy and not the other.

const USERSCRIPT = fs.readFileSync(
    path.join(__dirname, '..', 'claude-usage-snapshot.user.js'),
    'utf8',
);

// Pull named top-level declarations out of the IIFE and evaluate them in
// isolation. Anchored on the exact declaration so a rename fails loudly
// here rather than silently skipping the comparison. Function bodies end
// at the first line that is exactly the IIFE's four-space-indented "}".
function extractConst(name) {
    const m = USERSCRIPT.match(new RegExp(`^    const ${name} = [^;]*;`, 'm'));
    assert.ok(m, `${name} not found in the userscript`);
    return m[0];
}

function extractFunction(name) {
    const m = USERSCRIPT.match(new RegExp(`^    function ${name}\\([^)]*\\) \\{[\\s\\S]*?\\n {4}\\}`, 'm'));
    assert.ok(m, `function ${name} not found in the userscript`);
    return m[0];
}

function evalInlined(consts, functions, exportNames) {
    const src = [...consts.map(extractConst), ...functions.map(extractFunction)].join('\n');
    // eslint-disable-next-line no-new-func
    return new Function(`${src}\nreturn { ${exportNames.join(', ')} };`)();
}

const LABEL_INPUTS = [
    'Fable', 'fable', 'FABLE', '  Fable\n', 'Fable only', 'Fable 5',
    'FableNew', 'Fable this week', 'All models', 'This week', 'This weekMax (20x)',
    'Sonnet only', 'Claude Design', 'Current session', 'current session (5 hours)',
    'Claude Code', 'Chats', 'Cowork', 'Other', 'Usage credits',
    'Enable fable alerts', '', '   ', null, undefined, 77,
];

const HEADING_INPUTS = [
    'Plan usage limits', 'Plan usage limitsMax (20x)', 'Your usage limits', 'Your usage limitsTeam',
    'Weekly limits', 'Weekly limitsMax (20x)', 'Your usage', 'Your usageMax (20x)', '  Your usage\n',
    'Usage', 'Usage credits', 'Monthly spend limit', 'This week’s usage by product',
    'Additional features', '', null, undefined, 3,
];

test('inlined row and section helpers match lib/rows.js', () => {
    const inlined = evalInlined(
        ['FABLE_ROW_LABEL_PREFIXES', 'SESSION_ROW_LABEL_PREFIXES', 'WEEKLY_ROW_LABEL_PREFIXES',
            'SESSION_HEADINGS', 'WEEKLY_HEADINGS', 'COMBINED_HEADINGS'],
        ['_hasPrefix', 'isFableRowLabel', 'classifyUsageRow', 'classifySection'],
        ['isFableRowLabel', 'classifyUsageRow', 'classifySection'],
    );

    for (const input of LABEL_INPUTS) {
        assert.strictEqual(inlined.isFableRowLabel(input), rows.isFableRowLabel(input),
            `isFableRowLabel disagrees on ${JSON.stringify(input)}`);
        assert.strictEqual(inlined.classifyUsageRow(input), rows.classifyUsageRow(input),
            `classifyUsageRow disagrees on ${JSON.stringify(input)}`);
    }
    for (const input of HEADING_INPUTS) {
        assert.strictEqual(inlined.classifySection(input), rows.classifySection(input),
            `classifySection disagrees on ${JSON.stringify(input)}`);
    }
});

test('inlined prefix and heading lists match lib/rows.js', () => {
    for (const name of ['FABLE_ROW_LABEL_PREFIXES', 'SESSION_ROW_LABEL_PREFIXES', 'WEEKLY_ROW_LABEL_PREFIXES',
        'SESSION_HEADINGS', 'WEEKLY_HEADINGS', 'COMBINED_HEADINGS']) {
        const m = USERSCRIPT.match(new RegExp(`^    const ${name} = (\\[[^\\]]*\\]);`, 'm'));
        assert.ok(m, `${name} not found in the userscript`);
        assert.deepStrictEqual(JSON.parse(m[1].replace(/'/g, '"')), rows[name], name);
    }
});

test('inlined reset parsers match lib/resets.js', () => {
    const inlined = evalInlined(
        ['WEEKDAYS', 'WEEKDAY_CLOCK_RE'],
        ['parseWeekdayClock', 'nearestWeekdayClockMs', 'parseSessionEnds', 'parseWeeklyEnds'],
        ['parseWeekdayClock', 'parseSessionEnds', 'parseWeeklyEnds'],
    );

    const texts = [
        'Resets Thu 3:50 AM', 'Resets Thursday 11:00 PM', 'Resets sat 12:30 pm', 'Resets Sun 12:00 AM',
        'Resets in 3 hr 33 min', 'Resets in 19 min', 'Resets in 5 hr', 'Resets in 0 min',
        'Resets May 1', 'Starts when a message is sent', '', null, undefined,
    ];
    const bases = [
        new Date(2026, 8, 16, 23, 0, 0, 0).getTime(),
        new Date(2026, 8, 17, 4, 10, 0, 0).getTime(),
        new Date(2026, 8, 17, 23, 0, 0, 0).getTime(),
    ];
    for (const t of texts) {
        assert.deepStrictEqual(inlined.parseWeekdayClock(t), resets.parseWeekdayClock(t),
            `parseWeekdayClock disagrees on ${JSON.stringify(t)}`);
        for (const b of bases) {
            assert.strictEqual(inlined.parseSessionEnds(t, b), resets.parseSessionEnds(t, b),
                `parseSessionEnds disagrees on ${JSON.stringify(t)} @ ${b}`);
            assert.strictEqual(inlined.parseWeeklyEnds(t, b), resets.parseWeeklyEnds(t, b),
                `parseWeeklyEnds disagrees on ${JSON.stringify(t)} @ ${b}`);
        }
    }
});

test('inlined shouldSend matches lib/dedup.js', () => {
    const inlined = evalInlined(
        ['HEARTBEAT_MS'],
        ['_absentAsNull', 'shouldSend'],
        ['shouldSend', 'HEARTBEAT_MS'],
    );
    assert.strictEqual(inlined.HEARTBEAT_MS, dedup.HEARTBEAT_MS);

    const base = 1714200000000;
    const state = {
        lastSentAtMs: base, lastPercent: 42, lastResetText: 'Resets Thu 3:50 AM',
        lastWindowEndsMs: base + 3600000, lastSessionActive: undefined, lastWeeklyActive: undefined,
        lastFablePercent: 11,
    };
    const observations = [
        { sessionUsed: 42, resetText: 'Resets Thu 3:50 AM', fableWeeklyUsed: 11, lastUpdatedAgeMs: null },
        { sessionUsed: 43, resetText: 'Resets Thu 3:50 AM', fableWeeklyUsed: 11, lastUpdatedAgeMs: null },
        { sessionUsed: 42, resetText: 'Resets Thu 8:50 AM', fableWeeklyUsed: 11, lastUpdatedAgeMs: null },
        { sessionUsed: 42, resetText: 'Resets Thu 3:50 AM', fableWeeklyUsed: 12, lastUpdatedAgeMs: null },
        { sessionUsed: 42, resetText: null, sessionActive: false, fableWeeklyUsed: 11, lastUpdatedAgeMs: 60000 },
    ];
    const clocks = [undefined, base + 1000, base + dedup.HEARTBEAT_MS, base + 2 * dedup.HEARTBEAT_MS];
    for (const obs of observations) {
        for (const now of clocks) {
            assert.strictEqual(
                inlined.shouldSend(obs, state, 120000, now),
                dedup.shouldSend(obs, state, 120000, now),
                `shouldSend disagrees on ${JSON.stringify(obs)} @ ${now}`,
            );
        }
    }
});

// The snapshot body is the contract with the server (internal/server/snapshot.go).
// Guard the field name specifically: a typo here fails silently -- the server
// ignores unknown JSON fields, the column stays NULL, and the chart just
// never grows a second line.
test('userscript posts the fable value under the field the server reads', () => {
    assert.match(USERSCRIPT, /body\.fable_weekly_used = extracted\.fableWeeklyUsed;/);
});

test('userscript passes a wall clock to shouldSend so the heartbeat can fire', () => {
    assert.match(USERSCRIPT, /shouldSend\(extracted, prevState, lastObservedAgeMs, Date\.now\(\)\)/);
});
