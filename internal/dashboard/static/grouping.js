'use strict';

// Split a time-ordered series of points into one polyline per session.
// A point starts a new polyline whenever its `continuous_with_prev` is
// not strictly true (false, missing, or NULL→false from the backend).
// Single-element groups are still returned so the caller can render
// them as standalone dots.
//
// This file is the single source of truth for the grouping rule. It is
// loaded by the dashboard via <script src="grouping.js"></script> and
// require()-able from Node tests via the CommonJS shim at the bottom.
function groupPolylines(points) {
    const groups = [];
    let cur = null;
    for (const p of points) {
        if (!cur || p.continuous_with_prev !== true) {
            cur = [];
            groups.push(cur);
        }
        cur.push(p);
    }
    return groups;
}

if (typeof module !== 'undefined') {
    module.exports = { groupPolylines };
}
