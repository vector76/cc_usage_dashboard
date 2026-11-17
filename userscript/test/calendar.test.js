'use strict';

// Calendar periods for /report and /projects. The DST cases need a zone that
// observes it, so this file pins TZ before any Date is built; Node reads TZ
// lazily, on first use.
process.env.TZ = 'America/New_York';

const test = require('node:test');
const assert = require('node:assert');

const {
    startOfDay, addDays, startOfWeek, periodBoundaries, rangeBoundaries,
} = require('../../internal/dashboard/static/calendar');

const local = (y, m, d, h = 0, min = 0) => new Date(y, m - 1, d, h, min);
const ymd = (d) => [d.getFullYear(), d.getMonth() + 1, d.getDate(), d.getHours(), d.getMinutes()];

test('startOfWeek is the Sunday on or before, for every day of the week', () => {
    // 2026-11-15 is a Sunday.
    for (let i = 0; i < 7; i++) {
        const d = local(2026, 11, 15 + i, 15, 30);
        assert.deepStrictEqual(ymd(startOfWeek(d)), [2026, 11, 15, 0, 0], `day ${i}`);
    }
    assert.deepStrictEqual(ymd(startOfWeek(local(2026, 11, 14, 23, 59))), [2026, 11, 8, 0, 0]);
});

test('startOfWeek crosses month and year boundaries', () => {
    // 2027-01-01 is a Friday; its week began Sunday 2026-12-27.
    assert.deepStrictEqual(ymd(startOfWeek(local(2027, 1, 1, 9))), [2026, 12, 27, 0, 0]);
});

test('day boundaries stay on local midnight across DST changes', () => {
    // US DST ends 2026-11-01 (a 25-hour day) and starts 2026-03-08 (23 hours).
    const fall = periodBoundaries('day', 3, local(2026, 11, 2, 12));
    assert.deepStrictEqual(fall.map(ymd), [
        [2026, 10, 31, 0, 0], [2026, 11, 1, 0, 0], [2026, 11, 2, 0, 0], [2026, 11, 3, 0, 0],
    ]);
    assert.strictEqual(fall[2] - fall[1], 25 * 3600e3);

    const spring = rangeBoundaries('day', local(2026, 3, 8), local(2026, 3, 8));
    assert.strictEqual(spring.length, 2);
    assert.strictEqual(spring[1] - spring[0], 23 * 3600e3);
});

test('periodBoundaries ends with the current, partial period', () => {
    const now = local(2026, 11, 17, 10, 15); // a Tuesday
    const weeks = periodBoundaries('week', 2, now);
    assert.deepStrictEqual(weeks.map(ymd), [
        [2026, 11, 8, 0, 0], [2026, 11, 15, 0, 0], [2026, 11, 22, 0, 0],
    ]);
    const days = periodBoundaries('day', 1, now);
    assert.deepStrictEqual(days.map(ymd), [[2026, 11, 17, 0, 0], [2026, 11, 18, 0, 0]]);
});

test('rangeBoundaries covers whole periods touching both ends', () => {
    const b = rangeBoundaries('week', local(2026, 11, 3), local(2026, 11, 10));
    assert.deepStrictEqual(b.map(ymd), [
        [2026, 11, 1, 0, 0], [2026, 11, 8, 0, 0], [2026, 11, 15, 0, 0],
    ]);
    const d = rangeBoundaries('day', local(2026, 11, 30, 18), local(2026, 12, 1, 1));
    assert.deepStrictEqual(d.map(ymd), [
        [2026, 11, 30, 0, 0], [2026, 12, 1, 0, 0], [2026, 12, 2, 0, 0],
    ]);
});

test('helpers', () => {
    assert.deepStrictEqual(ymd(startOfDay(local(2026, 2, 3, 22, 1))), [2026, 2, 3, 0, 0]);
    assert.deepStrictEqual(ymd(addDays(local(2026, 2, 27), 2)), [2026, 3, 1, 0, 0]);
});
