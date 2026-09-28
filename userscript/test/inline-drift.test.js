'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const rows = require('../lib/rows');
const resets = require('../lib/resets');
const dedup = require('../lib/dedup');
const bars = require('../lib/bars');
const continuity = require('../lib/continuity');
const route = require('../lib/route');
const state = require('../lib/state');
const visibility = require('../lib/visibility');

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
    const m = USERSCRIPT.match(new RegExp(`^    const ${name} =\\s[^;]*;`, 'm'));
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
        ['WEEKDAYS', 'WEEKDAY_CLOCK_RE', 'WEEKDAY_CLOCK_ALT_RE'],
        ['parseWeekdayClock', 'nearestWeekdayClockMs', 'parseSessionEnds', 'parseWeeklyEnds'],
        ['parseWeekdayClock', 'parseSessionEnds', 'parseWeeklyEnds'],
    );

    const texts = [
        'Resets Thu 3:50 AM', 'Resets Thursday 11:00 PM', 'Resets sat 12:30 pm', 'Resets Sun 12:00 AM',
        'Resets Thu 15:50', 'Resets Thursday, 23:00', 'Resets Thu, 3:50 PM', 'Resets Thu 3:50 p.m.',
        'Resets Thu 24:00', 'Resets Thu 15:60',
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

test('inlined usage-bar recognition matches lib/bars.js', () => {
    const inlined = evalInlined(['USAGE_BAR_SELECTOR'], ['isUsageBarTarget'], ['USAGE_BAR_SELECTOR', 'isUsageBarTarget']);
    assert.strictEqual(inlined.USAGE_BAR_SELECTOR, bars.USAGE_BAR_SELECTOR);
    for (const role of ['meter', 'progressbar', 'slider', '', null, undefined]) {
        for (const label of ['Usage', 'Usage credits', 'usage', '', null, undefined]) {
            assert.strictEqual(inlined.isUsageBarTarget(role, label), bars.isUsageBarTarget(role, label),
                `isUsageBarTarget disagrees on ${JSON.stringify([role, label])}`);
        }
    }
});

test('inlined decideContinuity matches lib/continuity.js', () => {
    const inlined = evalInlined(
        ['WALL_CLOCK_GAP_MS', 'WINDOW_ENDS_JUMP_MS'],
        ['decideContinuity'],
        ['decideContinuity', 'WALL_CLOCK_GAP_MS', 'WINDOW_ENDS_JUMP_MS'],
    );
    assert.strictEqual(inlined.WALL_CLOCK_GAP_MS, continuity.WALL_CLOCK_GAP_MS);
    assert.strictEqual(inlined.WINDOW_ENDS_JUMP_MS, continuity.WINDOW_ENDS_JUMP_MS);

    const base = 1714200000000;
    const states = [
        null,
        { lastSentAtMs: base, lastPercent: 42, lastWindowEndsMs: base + 3600000 },
        { lastSentAtMs: base, lastPercent: 42, lastWindowEndsMs: null },
    ];
    const observations = [
        { percent: 42, windowEndsMs: base + 3600000 },
        { percent: 43, windowEndsMs: base + 3600000 + continuity.WINDOW_ENDS_JUMP_MS + 1 },
        { percent: 41, windowEndsMs: base + 3600000 },
        { percent: 42, windowEndsMs: null },
        { percent: null, windowEndsMs: undefined },
    ];
    const clocks = [base + 1000, base + continuity.WALL_CLOCK_GAP_MS, base + continuity.WALL_CLOCK_GAP_MS + 1];
    for (const prev of states) {
        for (const obs of observations) {
            for (const now of clocks) {
                assert.strictEqual(inlined.decideContinuity(obs, prev, now), continuity.decideContinuity(obs, prev, now),
                    `decideContinuity disagrees on ${JSON.stringify([obs, prev, now])}`);
            }
        }
    }
});

test('inlined isUsageRoute matches lib/route.js', () => {
    const inlined = evalInlined(['USAGE_PATH'], ['isUsageRoute'], ['isUsageRoute']);
    const pathnames = ['/settings/usage', '/settings/usage/', '/settings', '/new', '/chat/abc', '', null, undefined];
    const hashes = ['#settings/usage', '#/settings/usage', '#settings/usage/x', '#settings/usage?x=1',
        '#settings/usage-summary', '#settings', 'settings/usage', '', null, undefined];
    for (const p of pathnames) {
        for (const h of hashes) {
            assert.strictEqual(inlined.isUsageRoute(p, h), route.isUsageRoute(p, h),
                `isUsageRoute disagrees on ${JSON.stringify([p, h])}`);
        }
    }
});

function memoryStorage(initial) {
    const map = new Map(initial === undefined ? [] : [[state.STATE_STORAGE_KEY, initial]]);
    return {
        getItem: k => (map.has(k) ? map.get(k) : null),
        setItem: (k, v) => map.set(k, String(v)),
        dump: () => Object.fromEntries(map),
    };
}

// The inlined copies read globalThis.localStorage directly where lib/ goes
// through a test seam; shadow globalThis so neither touches the real one.
function inlinedState(storage) {
    const src = [extractConst('STATE_STORAGE_KEY'), extractFunction('loadState'), extractFunction('recordSentState')].join('\n');
    // eslint-disable-next-line no-new-func
    return new Function('globalThis', `${src}\nreturn { STATE_STORAGE_KEY, loadState, recordSentState };`)(
        { localStorage: storage },
    );
}

test('inlined loadState/recordSentState match lib/state.js', () => {
    assert.strictEqual(inlinedState(memoryStorage()).STATE_STORAGE_KEY, state.STATE_STORAGE_KEY);

    const raws = [
        undefined, 'not json', 'null', '"str"', '{}', '{"lastSentAtMs":"x"}',
        JSON.stringify({ lastSentAtMs: 1, lastPercent: 42, lastResetText: 'Resets in 3 hr', lastWindowEndsMs: 2 }),
        JSON.stringify({ lastSentAtMs: 1, lastPercent: 0, lastResetText: null, lastWindowEndsMs: null,
            lastFablePercent: 11, lastSessionActive: false, lastWeeklyActive: false }),
        JSON.stringify({ lastSentAtMs: 1, lastPercent: 5, lastFablePercent: null, lastSessionActive: true }),
    ];
    const records = [
        { sentAtMs: 1, percent: 42, resetText: 'Resets Thu 3:50 AM', windowEndsMs: 2 },
        { sentAtMs: 1, percent: 0, resetText: null, windowEndsMs: null, sessionActive: false, weeklyActive: false, fablePercent: 7 },
        { sentAtMs: 1, percent: null, resetText: undefined, windowEndsMs: undefined, fablePercent: null },
    ];
    try {
        for (const raw of raws) {
            const libStorage = memoryStorage(raw);
            state._setStorageForTests(libStorage);
            assert.deepStrictEqual(inlinedState(memoryStorage(raw)).loadState(), state.loadState(),
                `loadState disagrees on ${raw}`);
        }
        for (const rec of records) {
            const libStorage = memoryStorage();
            const inlStorage = memoryStorage();
            state._setStorageForTests(libStorage);
            state.recordSentState(rec);
            inlinedState(inlStorage).recordSentState(rec);
            assert.deepStrictEqual(inlStorage.dump(), libStorage.dump(),
                `recordSentState disagrees on ${JSON.stringify(rec)}`);
        }
    } finally {
        state._setStorageForTests(null);
    }
});

function stubDocument({ lockHidden = false } = {}) {
    const doc = { listeners: [] };
    doc.addEventListener = (type, fn, capture) => doc.listeners.push({ type, fn, capture });
    if (lockHidden) Object.defineProperty(doc, 'hidden', { value: true, configurable: false });
    return doc;
}

function spoofOutcome(install, opts) {
    const doc = stubDocument(opts);
    install(doc);
    const calls = [];
    const event = {
        stopImmediatePropagation: () => calls.push('stopImmediatePropagation'),
        stopPropagation: () => calls.push('stopPropagation'),
    };
    for (const l of doc.listeners) l.fn(event);
    return {
        hidden: doc.hidden,
        visibilityState: doc.visibilityState,
        webkitHidden: doc.webkitHidden,
        webkitVisibilityState: doc.webkitVisibilityState,
        listeners: doc.listeners.map(l => [l.type, l.capture]),
        calls,
    };
}

test('inlined installVisibilitySpoof matches lib/visibility.js', () => {
    const inlined = evalInlined([], ['installVisibilitySpoof'], ['installVisibilitySpoof']);
    for (const opts of [{}, { lockHidden: true }]) {
        assert.deepStrictEqual(spoofOutcome(inlined.installVisibilitySpoof, opts),
            spoofOutcome(visibility.installVisibilitySpoof, opts), `spoof disagrees on ${JSON.stringify(opts)}`);
    }
    assert.doesNotThrow(() => inlined.installVisibilitySpoof(null));
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
