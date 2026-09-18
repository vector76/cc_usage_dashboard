'use strict';

// The pure logic behind the dashboard's text readout — the compact lines
// that sit above and below the burn-down charts and say, in words, what the
// curves say in pictures. The charts and their tooltips are built inline in
// index.html; everything here is a plain value-in / value-out rule, kept in
// its own file so the Node tests can exercise the exact source the page runs.
//
// Loaded by the dashboard via <script src="summary.js"></script> and
// require()-able from tests via the CommonJS shim at the bottom.

// --- Allowance extrapolation ---------------------------------------------
//
// The dashboard knows two things about any period: how many dollars of usage
// it contains (summed from usage_events) and how much of the session /
// weekly percent gauge was burned over the same span (walked from the
// userscript's snapshots). Neither alone says what a window is worth, but
// together they do — if $20 moved the session gauge 50%, a whole session
// window is worth about $40.
//
// The estimate is deliberately a single division rather than a fit over the
// series. Both inputs already cover the whole selected period, so picking a
// longer period is what averages out a burst of unusually cheap or expensive
// traffic; there is nothing a curve fit would add.

// Below this much consumption the estimate is noise. The percent gauges are
// read off Anthropic's UI at whole- or near-whole-percent resolution, so a
// reading of 3% carries roughly ±0.5% of quantization error — a ±17% band on
// the resulting estimate, and worse the closer to zero it gets. At 10% that
// band is down to ±5%, which is honest enough to print.
const MIN_PCT_FOR_ESTIMATE = 10;

// extrapolateAllowance returns the dollar value of 100% of a window, given
// the dollars consumed over some period and the percent of that window's
// allowance consumed over the same period. Returns null when there is not
// enough signal to divide — the caller renders that as "insufficient data"
// rather than inventing a number.
//
// pct may exceed 100: a 30-day period spans many session windows, and "400%
// consumed for $160" is simply a better-measured $40 per window.
function extrapolateAllowance(consumedUsd, pct) {
    if (typeof consumedUsd !== 'number' || !isFinite(consumedUsd)) return null;
    if (typeof pct !== 'number' || !isFinite(pct)) return null;
    // No dollars means no measurement. This is a real state, not just a
    // defensive check: the percent gauges come from the userscript while the
    // dollars come from usage_events, so a half-configured install can report
    // percent movement with nothing spent. Dividing anyway would print a
    // confident $0.00 allowance.
    if (consumedUsd <= 0) return null;
    if (pct <= MIN_PCT_FOR_ESTIMATE) return null;
    return consumedUsd / (pct / 100);
}

// --- Slack-release flag ---------------------------------------------------

// slackReleaseState collapses the dashboard state into the flag shown beside
// the Burn-down heading: 'yes', 'no', 'paused' or 'unknown'.
//
// Paused outranks the gate verdict. Pausing stops the observations the
// headroom gates evaluate, so whatever they last concluded describes a stale
// world — including a stale 'yes'. Reporting 'no' there would read as a
// decision that slack is unavailable, when the truth is that the question
// cannot be answered until collection resumes. That is a real loss of
// information while paused, and the honest thing to show.
function slackReleaseState(s) {
    if (!s) return 'unknown';
    if (s.paused) return 'paused';
    if (s.slack_release_recommended == null) return 'unknown';
    return s.slack_release_recommended ? 'yes' : 'no';
}

// --- Age formatting -------------------------------------------------------

// fmtAge renders an age in seconds as the one or two coarsest units that
// still carry information: "47 s", "3 m 12 s", "2 h 5 m", "3 d 4 h". Used for
// the snapshot line, where the question is only ever "is this fresh enough to
// trust", so sub-unit precision would be clutter.
//
// Rounding happens before bucketing so 59.6 s reads "1 m 0 s" rather than the
// contradictory "60 s". A negative age is clock skew between the server's
// `now` and its own age field, not time travel, so it clamps to zero.
function fmtAge(sec) {
    const s = Math.max(0, Math.round(sec));
    if (s < 60) return s + ' s';
    if (s < 3600) return Math.floor(s / 60) + ' m ' + (s % 60) + ' s';
    if (s < 86400) return Math.floor(s / 3600) + ' h ' + Math.floor((s % 3600) / 60) + ' m';
    return Math.floor(s / 86400) + ' d ' + Math.floor((s % 86400) / 3600) + ' h';
}

if (typeof module !== 'undefined') {
    module.exports = {
        extrapolateAllowance,
        MIN_PCT_FOR_ESTIMATE,
        slackReleaseState,
        fmtAge,
    };
}
