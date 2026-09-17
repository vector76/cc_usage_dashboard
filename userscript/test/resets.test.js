'use strict';

const test = require('node:test');
const assert = require('node:assert');

const { parseSessionEnds, parseWeeklyEnds, parseWeekdayClock } = require('../lib/resets');

// All expectations are built with the local-time Date constructor so the
// tests pass in any timezone: the page renders local clock times and the
// parser converts them with the same local rules.
//
// 2026-09-16 is a Wednesday.
const WED_2300 = new Date(2026, 8, 16, 23, 0, 0, 0).getTime();
const THU_0350 = new Date(2026, 8, 17, 3, 50, 0, 0).getTime();
const THU_0410 = new Date(2026, 8, 17, 4, 10, 0, 0).getTime();
const THU_2300 = new Date(2026, 8, 17, 23, 0, 0, 0).getTime();

test('parseWeekdayClock: short and long weekday names, 12-hour clock', () => {
    assert.deepStrictEqual(parseWeekdayClock('Resets Thu 3:50 AM'), { dow: 4, hour: 3, minute: 50 });
    assert.deepStrictEqual(parseWeekdayClock('Resets Thursday 11:00 PM'), { dow: 4, hour: 23, minute: 0 });
    assert.deepStrictEqual(parseWeekdayClock('Resets Sun 12:00 AM'), { dow: 0, hour: 0, minute: 0 });
    assert.deepStrictEqual(parseWeekdayClock('Resets sat 12:30 pm'), { dow: 6, hour: 12, minute: 30 });
});

test('parseWeekdayClock: relative hints and garbage are not clock times', () => {
    assert.strictEqual(parseWeekdayClock('Resets in 3 hr 33 min'), null);
    assert.strictEqual(parseWeekdayClock('Resets May 1'), null);
    assert.strictEqual(parseWeekdayClock(''), null);
    assert.strictEqual(parseWeekdayClock(null), null);
});

test('parseSessionEnds: relative form (through August 2026) is unchanged', () => {
    const base = WED_2300;
    assert.strictEqual(parseSessionEnds('Resets in 3 hr 33 min', base),
        new Date(base + (3 * 60 + 33) * 60 * 1000).toISOString());
    assert.strictEqual(parseSessionEnds('Resets in 19 min', base),
        new Date(base + 19 * 60 * 1000).toISOString());
    assert.strictEqual(parseSessionEnds('Resets in 5 hr', base),
        new Date(base + 5 * 60 * 60 * 1000).toISOString());
    assert.strictEqual(parseSessionEnds('Resets in 0 min', base), null);
});

test('parseSessionEnds: absolute form (September 2026) resolves to the upcoming occurrence', () => {
    assert.strictEqual(parseSessionEnds('Resets Thu 3:50 AM', WED_2300), new Date(THU_0350).toISOString());
    assert.strictEqual(parseSessionEnds('Resets Thursday 3:50 AM', WED_2300), new Date(THU_0350).toISOString());
});

test('parseSessionEnds: a reset that just passed on a stale page stays in the recent past', () => {
    // 20 minutes after the displayed reset. The server tolerates an end up
    // to an hour in the past; jumping to next Thursday would be rejected
    // as too far in the future and would misrepresent the window anyway.
    assert.strictEqual(parseSessionEnds('Resets Thu 3:50 AM', THU_0410), new Date(THU_0350).toISOString());
});

test('parseSessionEnds: nearest occurrence, not next-in-week, across the weekday wrap', () => {
    // Base is Thursday 23:00; "Sun 12:00 AM" is the coming Sunday, 3 days ahead.
    const SUN_0000 = new Date(2026, 8, 20, 0, 0, 0, 0).getTime();
    assert.strictEqual(parseSessionEnds('Resets Sun 12:00 AM', THU_2300), new Date(SUN_0000).toISOString());
});

test('parseSessionEnds: unparseable text is null', () => {
    assert.strictEqual(parseSessionEnds(null, WED_2300), null);
    assert.strictEqual(parseSessionEnds('', WED_2300), null);
    assert.strictEqual(parseSessionEnds('Starts when a message is sent', WED_2300), null);
    assert.strictEqual(parseSessionEnds('Resets May 1', WED_2300), null);
});

test('parseWeeklyEnds: next future occurrence of the weekday clock time', () => {
    assert.strictEqual(parseWeeklyEnds('Resets Thu 11:00 PM', WED_2300), new Date(THU_2300).toISOString());
    assert.strictEqual(parseWeeklyEnds('Resets Thursday 11:00 PM', WED_2300), new Date(THU_2300).toISOString());
});

test('parseWeeklyEnds: a reset earlier today rolls to next week, never the past', () => {
    // Thursday 23:00 shown at Thursday 04:10 is later today; shown at
    // Thursday 23:00 exactly it has to be the *next* Thursday.
    assert.strictEqual(parseWeeklyEnds('Resets Thu 11:00 PM', THU_0410), new Date(THU_2300).toISOString());
    const NEXT_THU_2300 = new Date(2026, 8, 24, 23, 0, 0, 0).getTime();
    assert.strictEqual(parseWeeklyEnds('Resets Thu 11:00 PM', THU_2300), new Date(NEXT_THU_2300).toISOString());
});

test('parseWeeklyEnds: unparseable text is null', () => {
    assert.strictEqual(parseWeeklyEnds('Resets in 3 hr', WED_2300), null);
    assert.strictEqual(parseWeeklyEnds('Resets May 1', WED_2300), null);
    assert.strictEqual(parseWeeklyEnds(null, WED_2300), null);
});
