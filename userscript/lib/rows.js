'use strict';

// Pure helpers for identifying which usage section a bar sits under and
// which row it is *within* that section.
//
// Section-heading anchoring (see ../claude-usage-snapshot.user.js,
// precedingHeading) tells us which section a usage bar belongs to. Through
// August 2026 that was deliberately all it told us: heading text was the
// durable part of the page, row labels were not, and every section we
// cared about had exactly one bar we wanted -- the first one.
//
// Two revisions eroded that:
//
//   - July 2026: the "Weekly limits" section grew an aggregate row ("All
//     models") plus a per-model sub-row ("Fable"), distinguishable only by
//     the row label:
//
//       <div data-cds="Meter">
//         <div role="meter" aria-valuenow="77" aria-labelledby="_r_f0_">…</div>
//       </div>
//
//     where the aria-labelledby target's text is literally "Fable". The
//     bar's own data-variant/bg-* classes are NOT usable instead: they
//     encode a threshold (accent -> warning as the bar approaches its
//     limit), not identity.
//
//   - September 2026: the session and weekly sections were merged into a
//     single "Your usage" section (h2) holding "Current session", "This
//     week" and "Fable this week" rows, followed by unrelated sections
//     ("Usage credits", "This week's usage by product") whose meters we
//     must ignore. With one heading for three rows there is no positional
//     rule left; every row in the combined section is claimed by label.
//
// Matching is by case-folded prefix against a list of accepted forms,
// mirroring how section headings are matched. That absorbs the cosmetic
// edits Anthropic makes most often -- a version suffix ("Fable 5"), a
// qualifier ("Fable only"), a trailing badge concatenated by the design
// system ("This weekMax (20x)"). "Your usage" is itself a prefix of the
// legacy "Your usage limits", so classifySection tests the legacy, more
// specific forms first.
//
// When a label match fails the consequence is contained: that field goes
// absent from the snapshot, the server writes NULL, and the dashboard draws
// no line for it. In the legacy sections the session and weekly aggregate
// rows are still selected positionally and cannot be collateral damage.
//
// This file is the single source of truth. Its body is also inlined into
// ../claude-usage-snapshot.user.js so Tampermonkey runs without a build
// step; tests load it via require() from here.

// Accepted prefixes, lower-case, for the rows we read.
const FABLE_ROW_LABEL_PREFIXES = ['fable'];
const SESSION_ROW_LABEL_PREFIXES = ['current session'];
const WEEKLY_ROW_LABEL_PREFIXES = ['this week', 'all models'];

// Section headings that anchor extraction, matched as a prefix so a
// trailing plan-tier badge ("Your usage limitsTeam") doesn't break it.
//
// Known history:
//   "Plan usage limits" + "Weekly limits" -- through April 2026
//   "Your usage limits" + "Weekly limits" -- May 2026 through August 2026
//   "Your usage" (combined)               -- observed September 2026
const SESSION_HEADINGS = ['Your usage limits', 'Plan usage limits'];
const WEEKLY_HEADINGS = ['Weekly limits'];
const COMBINED_HEADINGS = ['Your usage'];

function _hasPrefix(text, prefixes) {
    if (typeof text !== 'string') return false;
    const t = text.trim().toLowerCase();
    if (!t) return false;
    return prefixes.some(p => t.startsWith(p.toLowerCase()));
}

// isFableRowLabel answers "is this the Fable sub-row?" given the row's
// resolved accessible name. Null/undefined/non-string input is not a match,
// so a bar with no resolvable label falls through to the positional path.
function isFableRowLabel(label) {
    return _hasPrefix(label, FABLE_ROW_LABEL_PREFIXES);
}

// classifyUsageRow maps a row's accessible name to 'session' | 'weekly' |
// 'fable', or null for any row we do not read (per-product breakdown,
// usage credits, future additions). Fable is tested first because its
// September 2026 label ("Fable this week") would otherwise never be
// confused with the aggregate but documents the precedence explicitly.
function classifyUsageRow(label) {
    if (isFableRowLabel(label)) return 'fable';
    if (_hasPrefix(label, SESSION_ROW_LABEL_PREFIXES)) return 'session';
    if (_hasPrefix(label, WEEKLY_ROW_LABEL_PREFIXES)) return 'weekly';
    return null;
}

// classifySection maps a heading's text to 'session' | 'weekly' |
// 'combined', or null for headings we ignore. Legacy forms are tested
// before the combined form because "Your usage" prefixes "Your usage
// limits".
function classifySection(heading) {
    if (_hasPrefix(heading, SESSION_HEADINGS)) return 'session';
    if (_hasPrefix(heading, WEEKLY_HEADINGS)) return 'weekly';
    if (_hasPrefix(heading, COMBINED_HEADINGS)) return 'combined';
    return null;
}

if (typeof module !== 'undefined') {
    module.exports = {
        FABLE_ROW_LABEL_PREFIXES,
        SESSION_ROW_LABEL_PREFIXES,
        WEEKLY_ROW_LABEL_PREFIXES,
        SESSION_HEADINGS,
        WEEKLY_HEADINGS,
        COMBINED_HEADINGS,
        isFableRowLabel,
        classifyUsageRow,
        classifySection,
    };
}
