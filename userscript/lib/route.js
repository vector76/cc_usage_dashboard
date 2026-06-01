'use strict';

// Pure predicate: do this (pathname, hash) pair indicate the user is
// viewing the usage settings tab?
//
// Route history:
//   - Through ~May 2026 the usage page was a real path, "/settings/usage"
//     (full-page settings).
//   - As of June 2026 settings is a hash-routed modal dialog rendered over
//     the current page, e.g. "https://claude.ai/new#settings/usage". The
//     pathname is then "/new" (or any chat/page path) and the route lives
//     in the fragment as "#settings/usage".
//
// We accept either form so the predicate keeps meaning "the user intends
// to view usage." That intent — not merely "a progressbar is present" — is
// what lets a *missing* usage progressbar be reported as a genuine parse
// error rather than silently ignored on every unrelated page.
//
// No DOM access — callers pass location.pathname / location.hash.
//
// This file is the single source of truth. Its body is also inlined into
// ../claude-usage-snapshot.user.js so Tampermonkey runs without a build
// step; tests load it via require() from here.

function isUsageRoute(pathname, hash) {
    if (pathname === '/settings/usage') return true;
    // Strip the leading '#'. The hash route mirrors the old path
    // ("settings/usage"), tolerating an optional leading slash and a
    // trailing sub-path or query. The [/?]|$ boundary stops a sibling
    // like "settings/usage-summary" from matching.
    const route = String(hash || '').replace(/^#/, '');
    return /^\/?settings\/usage(?:[/?]|$)/.test(route);
}

if (typeof module !== 'undefined') {
    module.exports = { isUsageRoute };
}
