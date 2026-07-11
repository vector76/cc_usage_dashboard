'use strict';

// Pure helpers for recognizing a usage-bar element across claude.ai
// markup revisions.
//
// Markup history:
//   - Through early July 2026 each usage bar rendered as
//     <div role="progressbar" aria-label="Usage" aria-valuenow="…">.
//   - As of July 2026 the page uses a design-system Meter component:
//     <div data-cds="Meter"><div role="meter" aria-valuenow="…"
//     aria-labelledby="…">…</div></div> — role changed to "meter" and
//     there is no aria-label at all (the accessible name comes from
//     aria-labelledby pointing at the row-label span).
//
// The "Usage credits" section renders its own role="meter" bar
// (aria-label="Usage credits"). It is intentionally NOT excluded here:
// section-heading anchoring in the extractor already discards bars whose
// preceding heading is neither the session nor the weekly section, and
// keeping this predicate generation-agnostic means the next cosmetic
// aria-label tweak doesn't break it.
//
// USAGE_BAR_SELECTOR matches both generations for querySelector callers;
// isUsageBarTarget answers the same question for a MutationObserver
// target given its attribute values. No DOM access — callers pass
// getAttribute results.
//
// This file is the single source of truth. Its body is also inlined into
// ../claude-usage-snapshot.user.js so Tampermonkey runs without a build
// step; tests load it via require() from here.

const USAGE_BAR_SELECTOR =
    '[role="progressbar"][aria-label="Usage"], [role="meter"][aria-valuenow]';

function isUsageBarTarget(role, ariaLabel) {
    if (role === 'meter') return true;
    return role === 'progressbar' && ariaLabel === 'Usage';
}

if (typeof module !== 'undefined') {
    module.exports = { USAGE_BAR_SELECTOR, isUsageBarTarget };
}
