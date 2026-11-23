package dashboard

import (
	"database/sql"
	"time"
)

// Enterprise accounts have no session or weekly quota windows, so the two
// charts show a trailing span instead: 15h of 15-minute bars and 7d of 6-hour
// bars. Only cost bars are drawn; there is no %-used curve, pace line or slack
// zone because nothing reports a quota to draw them against.

// rollingBounds returns the trailing span for kind ("session" or "weekly").
//
// end is the first bucket boundary after now, so the bar still filling is the
// rightmost one, and start is end minus the span. Both sit on a grid phased to
// loc's wall clock (UTC offset mod bucket width), so bars fall on local
// :00/:15/:30/:45 and local 00/06/12/18. Because the grid is fixed, bars that
// have scrolled into the past keep their edges: the window only ever advances
// in whole buckets, never by a fraction that would re-slice old data.
func rollingBounds(kind string, now time.Time, loc *time.Location) (start, end time.Time, bucketSecs int) {
	if loc == nil {
		loc = time.Local
	}
	bucketSecs = bucketSecsForKind(kind)
	span := 15 * time.Hour
	if kind == "weekly" {
		span = 7 * 24 * time.Hour
	}
	_, offset := now.In(loc).Zone()
	b := int64(bucketSecs)
	local := now.Unix() + int64(offset)
	floor := local / b
	if local < 0 && local%b != 0 {
		floor-- // floor division for pre-epoch times
	}
	end = time.Unix((floor+1)*b-int64(offset), 0).UTC()
	return end.Add(-span), end, bucketSecs
}

// loadRollingWindow builds the enterprise-mode stand-in for a quota window:
// the trailing span from rollingBounds with its cost bars and nothing else.
func (h *Handler) loadRollingWindow(db *sql.DB, kind string) (*WindowState, error) {
	loc := h.loc
	if loc == nil {
		loc = time.Local
	}
	start, end, bucketSecs := rollingBounds(kind, h.now(), loc)

	// Bucket starts must satisfy unix == phase (mod bucketSecs) where local
	// time = unix + offset is a multiple of bucketSecs, so phase = -offset.
	_, offset := h.now().In(loc).Zone() // same offset rollingBounds used
	b := int64(bucketSecs)
	phase := ((-int64(offset))%b + b) % b

	volume, err := h.loadVolumeSeriesPhased(db, start, end, bucketSecs, phase)
	if err != nil {
		return nil, err
	}
	return &WindowState{
		Kind:       kind,
		StartedAt:  start,
		EndsAt:     end,
		Series:     []UsedSeriesPoint{},
		Volume:     volume,
		BucketSecs: bucketSecs,
		Rolling:    true,
	}, nil
}
