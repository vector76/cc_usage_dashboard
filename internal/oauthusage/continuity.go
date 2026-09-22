package oauthusage

import "time"

// Thresholds mirroring userscript/lib/continuity.js, so a snapshot from
// this source and one from the userscript agree on what counts as a break.
const (
	// wallClockGap is how long a silence may last before the next reading
	// starts a fresh segment rather than chaining off the last one.
	wallClockGap = 15 * time.Minute

	// windowEndsJump is how far a reset boundary may move and still be
	// considered the same window. Generous on purpose: the endpoint
	// recomputes resets_at per request, and a boundary that genuinely
	// changes moves by hours, not minutes.
	windowEndsJump = 1 * time.Hour
)

// decideContinuity reports whether cur can be linearly chained off prev.
//
// Scope limit, inherited deliberately from the userscript: this speaks
// only to *session* continuity. A weekly reset usually lands while the
// session percent sits at 0, so it cannot be seen from here; the renderer
// breaks independently on a strict decrease in whichever series it plots.
// Widening this to consider the weekly or Fable percent would be wrong in
// two ways — one flag cannot describe three curves, and a false value also
// suppresses write-time plateau compaction.
func decideContinuity(prev *Reading, prevAt time.Time, cur *Reading, curAt time.Time) bool {
	if prev == nil {
		return false
	}
	if curAt.Sub(prevAt) > wallClockGap {
		return false
	}
	if prev.SessionUsed != nil && cur.SessionUsed != nil && *cur.SessionUsed < *prev.SessionUsed {
		return false
	}
	if prev.SessionWindowEnds != nil && cur.SessionWindowEnds != nil {
		drift := cur.SessionWindowEnds.Sub(*prev.SessionWindowEnds)
		if drift < 0 {
			drift = -drift
		}
		if drift > windowEndsJump {
			return false
		}
	}
	return true
}
