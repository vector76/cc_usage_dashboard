'use strict';

// Pure helpers for identifying an individual row *within* a usage section.
//
// Section-heading anchoring (see ../claude-usage-snapshot.user.js,
// precedingHeading) tells us which section a usage bar belongs to, and that
// is deliberately all it tells us: heading text is the durable part of the
// page, row labels are not. It was sufficient while every section we cared
// about had exactly one bar we wanted — the first one.
//
// The "Weekly limits" section broke that. As of July 2026 it renders an
// aggregate row ("All models") plus one per-model sub-row ("Fable"), and the
// only thing distinguishing them is the row label:
//
//   <div data-cds="Meter">
//     <div role="meter" aria-valuenow="77" aria-labelledby="_r_f0_">…</div>
//   </div>
//
// where the aria-labelledby target's text is literally "Fable". There is no
// structural signal to use instead. In particular the bar's own
// data-variant/bg-* classes are NOT usable: they encode a threshold
// (accent → warning as the bar approaches its limit), not identity, so the
// Fable bar's `warning` styling is a fact about it being at 77% rather than
// about it being Fable.
//
// Matching is therefore by label, and is defensive about it: case-folded
// prefix match against a list of accepted forms, mirroring how
// SESSION_HEADINGS is matched. That absorbs the cosmetic edits Anthropic
// makes most often — a version suffix ("Fable 5"), a qualifier ("Fable
// only", by analogy with the "Sonnet only" row that has appeared here), or
// a trailing badge concatenated by the design system.
//
// When the match fails the consequence is contained: the fable field goes
// absent from the snapshot, the server writes NULL, and the dashboard draws
// no fable line. The session and weekly aggregate rows are unaffected,
// because they are still selected positionally.
//
// This file is the single source of truth. Its body is also inlined into
// ../claude-usage-snapshot.user.js so Tampermonkey runs without a build
// step; tests load it via require() from here.

// Accepted prefixes for the per-model Fable sub-row, lower-case.
const FABLE_ROW_LABEL_PREFIXES = ['fable'];

// isFableRowLabel answers "is this the Fable sub-row?" given the row's
// resolved accessible name. Null/undefined/non-string input is not a match,
// so a bar with no resolvable label falls through to the positional path.
function isFableRowLabel(label) {
    if (typeof label !== 'string') return false;
    const t = label.trim().toLowerCase();
    if (!t) return false;
    return FABLE_ROW_LABEL_PREFIXES.some(p => t.startsWith(p));
}

if (typeof module !== 'undefined') {
    module.exports = { FABLE_ROW_LABEL_PREFIXES, isFableRowLabel };
}
