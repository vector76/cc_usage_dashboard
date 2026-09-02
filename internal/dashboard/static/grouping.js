'use strict';

// Split a time-ordered series of points into one polyline per session.
// A point starts a new polyline when EITHER rule fires:
//
//   1. Its `continuous_with_prev` is not strictly true (false, missing,
//      or NULL→false from the backend) — the source told us this point
//      does not chain off the previous one.
//   2. Its `percent_used` is strictly below the previous point's — the
//      quota this curve plots was reset between the two observations.
//
// Single-element groups are still returned so the caller can render
// them as standalone dots.
//
// Why rule 2 exists. `continuous_with_prev` is decided client-side by
// decideContinuity (userscript/lib/continuity.js), and the percent it
// compares is the *session* percent — one flag, computed from one of the
// several quotas the dashboard plots. A weekly reset (Anthropic dropping
// weekly_used from ~40% back to 0 mid-window) therefore arrives with the
// flag still true, because the session bar is typically parked at 0 while
// it happens and "0 < 0" is false. The weekly curve then draws straight
// through the reset as a diagonal sweeping up to the right, implying a
// continuous recovery of quota that never occurred.
//
// Rule 2 is the general form of what rule 1 was reaching for: whatever
// quantity a curve plots, a decrease in that quantity is a reset of it.
// Applying it here rather than adding a second per-quota continuity flag
// means it needs no schema change, needs no new observations, covers the
// weekly and Fable curves as well as the session one, and repairs history
// already on disk — the flag cannot be recomputed for snapshots that were
// recorded months ago, but their percentages are right there.
//
// Within a window these series are monotonically non-decreasing (quota
// only gets consumed), so a strict decrease is unambiguous. The one other
// thing that produces one — Anthropic raising a limit mid-window — is
// also a discontinuity, and also wants a break rather than a diagonal.
//
// This file is the single source of truth for the grouping rule. It is
// loaded by the dashboard via <script src="grouping.js"></script> and
// require()-able from Node tests via the CommonJS shim at the bottom.
function groupPolylines(points) {
    const groups = [];
    let cur = null;
    let prev = null;
    for (const p of points) {
        // Guarded on typeof so a point with a missing/non-numeric
        // percent_used never fabricates a break: absent data is "no
        // information", exactly as with the continuity flag.
        const quotaReset = prev !== null &&
            typeof p.percent_used === 'number' &&
            typeof prev.percent_used === 'number' &&
            p.percent_used < prev.percent_used;

        if (!cur || p.continuous_with_prev !== true || quotaReset) {
            cur = [];
            groups.push(cur);
        }
        cur.push(p);
        prev = p;
    }
    return groups;
}

// Model-family colors. Families come from the backend's ModelFamily()
// classifier (internal/consumption/breakdown.go). VOLUME_FAMILY_ORDER fixes the
// bottom-up stack order within a volume bar, the legend order, and the row order
// wherever a page groups by family. Shades are picked to stay distinct from each
// other and readable against the light background: blue / green / red / yellow /
// gray. Mythos deliberately reuses fable's red: it is the separately gated build
// of the same model, and an account uses one or the other, so the two never
// compete for attention in the same bar or table.
//
// These live here rather than in index.html because two pages render from them —
// the dashboard's stacked bars and the range report's per-model table — and a
// second copy is a second thing to forget when a family is added.
const VOLUME_FAMILY_ORDER = ["opus", "sonnet", "fable", "mythos", "haiku", "other"];
const VOLUME_FAMILY_COLORS = {
    opus: "#2563eb",   // blue
    sonnet: "#16a34a", // green
    fable: "#dc2626",  // red
    mythos: "#dc2626", // red, same as fable (see note above)
    haiku: "#eab308",  // yellow
    other: "#9ca3af",  // gray
};

if (typeof module !== 'undefined') {
    module.exports = { groupPolylines, VOLUME_FAMILY_ORDER, VOLUME_FAMILY_COLORS };
}
