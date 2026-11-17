'use strict';

// Calendar periods in the browser's local time zone, shared by /report and
// /projects so the two pages can never disagree about when a day or week
// starts.
//
// Days run local midnight to local midnight. Weeks run Sunday 00:00 to the
// following Sunday 00:00 (that is, Sunday through Saturday).
//
// Every boundary is built with the Date(y, m, d) constructor, which lands on
// local midnight even across a daylight-saving change. Adding 86,400,000 ms
// instead would drift an hour off midnight on the two days a year that are
// 23 or 25 hours long. The server receives the boundaries as absolute
// instants (toISOString) and never has to know the zone; see
// docs/usage-report.md "Timezone".

function startOfDay(d) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

function addDays(d, n) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
}

// startOfWeek returns the local midnight of the Sunday on or before d.
// getDay() is 0 on Sunday, so a Sunday is its own week start.
function startOfWeek(d) {
  return addDays(startOfDay(d), -d.getDay());
}

// periodBoundaries returns the bucket edges for `count` whole periods ending
// with the one containing `now`: count+1 local midnights, oldest first. The
// last edge is in the future; the current period is the partial one.
function periodBoundaries(unit, count, now) {
  const step = unit === 'week' ? 7 : 1;
  const current = unit === 'week' ? startOfWeek(now) : startOfDay(now);
  const out = [];
  for (let i = count - 1; i >= -1; i--) out.push(addDays(current, -i * step));
  return out;
}

// rangeBoundaries returns the edges of every whole period touching the local
// dates [from, to] inclusive: the period containing `from` through the period
// containing `to`.
function rangeBoundaries(unit, from, to) {
  const step = unit === 'week' ? 7 : 1;
  let cur = unit === 'week' ? startOfWeek(from) : startOfDay(from);
  const last = unit === 'week' ? startOfWeek(to) : startOfDay(to);
  const out = [cur];
  while (cur <= last) {
    cur = addDays(cur, step);
    out.push(cur);
  }
  return out;
}

if (typeof module !== 'undefined') {
  module.exports = { startOfDay, addDays, startOfWeek, periodBoundaries, rangeBoundaries };
}
