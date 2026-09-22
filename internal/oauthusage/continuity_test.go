package oauthusage

import (
	"testing"
	"time"
)

func pct(v float64) *float64    { return &v }
func at(t time.Time) *time.Time { return &t }

// TestDecideContinuity mirrors the rules the userscript applies
// (userscript/lib/continuity.js, decideContinuity) so both sources agree on
// what a discontinuity is. The server uses the flag for write-time plateau
// compaction and the dashboard uses it to decide where to break the
// burn-down polyline.
func TestDecideContinuity(t *testing.T) {
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	ends := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)

	prev := &Reading{SessionUsed: pct(10), SessionWindowEnds: at(ends)}

	cases := []struct {
		name   string
		prev   *Reading
		prevAt time.Time
		cur    *Reading
		curAt  time.Time
		want   bool
	}{
		{
			name:  "cold start has no previous reading",
			prev:  nil,
			cur:   &Reading{SessionUsed: pct(10), SessionWindowEnds: at(ends)},
			curAt: base,
			want:  false,
		},
		{
			name:   "normal tick",
			prev:   prev,
			prevAt: base,
			cur:    &Reading{SessionUsed: pct(11), SessionWindowEnds: at(ends)},
			curAt:  base.Add(3 * time.Minute),
			want:   true,
		},
		{
			name:   "flat plateau is still continuous",
			prev:   prev,
			prevAt: base,
			cur:    &Reading{SessionUsed: pct(10), SessionWindowEnds: at(ends)},
			curAt:  base.Add(3 * time.Minute),
			want:   true,
		},
		{
			name:   "wall-clock gap exceeds the threshold",
			prev:   prev,
			prevAt: base,
			cur:    &Reading{SessionUsed: pct(11), SessionWindowEnds: at(ends)},
			curAt:  base.Add(wallClockGap + time.Minute),
			want:   false,
		},
		{
			name:   "session percent decreased, so the window reset",
			prev:   prev,
			prevAt: base,
			cur:    &Reading{SessionUsed: pct(2), SessionWindowEnds: at(ends)},
			curAt:  base.Add(3 * time.Minute),
			want:   false,
		},
		{
			name:   "window boundary jumped",
			prev:   prev,
			prevAt: base,
			cur:    &Reading{SessionUsed: pct(11), SessionWindowEnds: at(ends.Add(5 * time.Hour))},
			curAt:  base.Add(3 * time.Minute),
			want:   false,
		},
		{
			// Sub-second jitter is already removed by normalizeResetsAt,
			// but a boundary that shifts by a minute or two must not be
			// read as a new window either.
			name:   "window boundary drifts within tolerance",
			prev:   prev,
			prevAt: base,
			cur:    &Reading{SessionUsed: pct(11), SessionWindowEnds: at(ends.Add(2 * time.Minute))},
			curAt:  base.Add(3 * time.Minute),
			want:   true,
		},
		{
			// Absent data is not evidence of a break. Reporting a start
			// here would suppress write-time plateau compaction for no
			// reason.
			name:   "missing session percent on either side",
			prev:   &Reading{SessionWindowEnds: at(ends)},
			prevAt: base,
			cur:    &Reading{SessionWindowEnds: at(ends)},
			curAt:  base.Add(3 * time.Minute),
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideContinuity(tc.prev, tc.prevAt, tc.cur, tc.curAt)
			if got != tc.want {
				t.Errorf("decideContinuity = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecideContinuityIgnoresWeeklyDecrease documents the same scope limit
// the userscript carries: the flag speaks only to session continuity. A
// weekly reset typically lands while the session bar sits at 0, so it
// cannot be detected here, and the renderer breaks independently on a
// strict decrease in whichever series it plots. Widening this to consider
// the weekly percent would also suppress plateau compaction, because one
// flag cannot describe three curves at once.
func TestDecideContinuityIgnoresWeeklyDecrease(t *testing.T) {
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	ends := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)

	prev := &Reading{SessionUsed: pct(0), WeeklyUsed: pct(98), SessionWindowEnds: at(ends)}
	cur := &Reading{SessionUsed: pct(0), WeeklyUsed: pct(1), SessionWindowEnds: at(ends)}

	if got := decideContinuity(prev, base, cur, base.Add(3*time.Minute)); !got {
		t.Error("a weekly-only reset must not set the session continuity flag to false")
	}
}
