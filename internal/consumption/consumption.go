// Package consumption reports raw usage over a period: dollar-equivalent
// cost, plus snapshot-derived percent-of-quota consumption for the session
// and weekly windows. The latter two can exceed 100% over a multi-window
// period (e.g. a 24h period spans roughly 5 session windows).
package consumption

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// Result is the JSON response from GET /consumption.
type Result struct {
	Period                 string    `json:"period"`
	PeriodStart            time.Time `json:"period_start"`
	PeriodEnd              time.Time `json:"period_end"`
	ConsumedUSDEquivalent  float64   `json:"consumed_usd_equivalent"`
	ConsumedSessionPct     *float64  `json:"consumed_session_pct"`
	ConsumedWeeklyPct      *float64  `json:"consumed_weekly_pct"`
	EventsTotal            int64     `json:"events_total"`
	EventsWithReportedCost int64     `json:"events_with_reported_cost"`
	EventsWithComputedCost int64     `json:"events_with_computed_cost"`
	EventsWithoutCost      int64     `json:"events_without_cost"`
}

// Calculator computes the consumption report.
type Calculator struct {
	db  *sql.DB
	now func() time.Time
}

func NewCalculator(db *sql.DB) *Calculator {
	return &Calculator{db: db, now: time.Now}
}

// SetNow injects a clock for tests.
func (c *Calculator) SetNow(fn func() time.Time) {
	c.now = fn
}

// Calculate computes the consumption report for the given period string
// (e.g. "24h", "7d"). Empty string defaults to "24h".
func (c *Calculator) Calculate(periodStr string) (*Result, error) {
	if periodStr == "" {
		periodStr = "24h"
	}
	duration, err := parsePeriod(periodStr)
	if err != nil {
		return nil, fmt.Errorf("invalid period: %w", err)
	}
	if duration < 0 {
		return nil, fmt.Errorf("invalid period: negative duration %q", periodStr)
	}
	endTime := c.now().UTC()
	startTime := endTime.Add(-duration)

	res := &Result{
		Period:      periodStr,
		PeriodStart: startTime,
		PeriodEnd:   endTime,
	}

	if err := c.aggregateEvents(res, startTime, endTime); err != nil {
		return nil, err
	}

	sessionPct, err := c.percentConsumed("session", startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("session %% consumed: %w", err)
	}
	res.ConsumedSessionPct = sessionPct

	weeklyPct, err := c.percentConsumed("weekly", startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("weekly %% consumed: %w", err)
	}
	res.ConsumedWeeklyPct = weeklyPct

	return res, nil
}

func (c *Calculator) aggregateEvents(res *Result, startTime, endTime time.Time) error {
	err := c.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN cost_usd_equivalent IS NOT NULL AND cost_source = 'reported' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_usd_equivalent IS NOT NULL AND cost_source = 'computed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_usd_equivalent IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(cost_usd_equivalent), 0)
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at <= ?
	`, store.FormatTime(startTime), store.FormatTime(endTime)).Scan(
		&res.EventsTotal,
		&res.EventsWithReportedCost,
		&res.EventsWithComputedCost,
		&res.EventsWithoutCost,
		&res.ConsumedUSDEquivalent,
	)
	if err != nil {
		return fmt.Errorf("failed to aggregate usage events: %w", err)
	}
	return nil
}

// snapshot is one observation of a window's percent-used value, paired
// with the persisted continuity flag. The flag is tri-state: an explicit
// true / false from the userscript, or NULL on rows written before
// migration v5 added the column. NULL is retained as a separate state
// because the session walker treats it as "unknown — fall back to the
// migration-conservative behavior", while an explicit false carries
// stronger signal that the walker can interrogate further.
type snapshot struct {
	observedAt         time.Time
	used               float64 // 0-100
	continuousWithPrev *bool   // nil = NULL in DB
}

// nullBoolFromInt collapses the SQLite NullInt64 representation of the
// `continuous_with_prev` column into the tri-state `*bool` carried on a
// snapshot: nil for NULL, &true for any non-zero int, &false for zero.
func nullBoolFromInt(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 != 0
	return &b
}

// percentConsumed walks the snapshots for the requested window kind and
// sums the per-snapshot increases in `*_used`. Between two adjacent
// snapshots, a continuation contributes the non-negative delta; a start
// contributes its raw `*_used` as a fresh window's worth. The unobserved
// tail of the prior window — between its last snapshot and the reset — is
// treated as zero; snapshots typically arrive right up to window end, so
// any missed tail is small.
//
// "Start" detection is per-kind, because the persisted
// `continuous_with_prev` flag is decided by the userscript from
// session-oriented signals (15-min wall-clock gap, session-percent
// decrease, session_window_ends jump). Applying that flag to the weekly
// walk would turn every session reset into a phantom weekly reset.
//
//   - session: a NULL flag (pre-migration row) is conservatively treated
//     as a start. An explicit `true` is a continuation. An explicit
//     `false` is only treated as a start when there is positive numeric
//     evidence of a reset (`curr.used < prev.used`); a `false` flag with
//     no numeric drop is a benign idle gap (the userscript marks any
//     >15-min wall-clock gap as `false`) and is treated as a
//     continuation. This catches the common case of "user stepped away
//     mid-session" without ignoring genuine resets.
//   - weekly: a start is signalled by a strict decrease in weekly_used.
//     Weekly resets always drop weekly_used from ~high to ~0; the flag is
//     ignored because none of its signals carry information about weekly
//     window boundaries. This assumes weekly_used doesn't jitter downward
//     between adjacent snapshots — the same stability the userscript's own
//     continuity check assumes about session_used.
//
// Anchor: the most recent snapshot at or before periodStart, if any.
// Without an anchor, the first in-period snapshot establishes the baseline
// and contributes nothing — under-reporting in that case is preferred over
// inventing a fictitious "0% at period start" prior anchor.
//
// Returns nil if no snapshots exist for the kind anywhere on or before
// periodEnd, signalling "couldn't measure" rather than 0.
func (c *Calculator) percentConsumed(kind string, startTime, endTime time.Time) (*float64, error) {
	usedCol := "session_used"
	isStart := func(prev, curr snapshot) bool {
		if curr.continuousWithPrev == nil {
			return true // pre-migration NULL → conservative start
		}
		if *curr.continuousWithPrev {
			return false // explicit continuation
		}
		// Explicit false: distinguish a real reset from a benign idle
		// gap by requiring a numeric drop in session_used.
		return curr.used < prev.used
	}
	if kind == "weekly" {
		usedCol = "weekly_used"
		isStart = func(prev, curr snapshot) bool { return curr.used < prev.used }
	}

	anchor, err := c.snapshotAtOrBefore(usedCol, startTime)
	if err != nil {
		return nil, err
	}
	inPeriod, err := c.snapshotsInRange(usedCol, startTime, endTime)
	if err != nil {
		return nil, err
	}

	if anchor == nil && len(inPeriod) == 0 {
		return nil, nil
	}

	walk := make([]snapshot, 0, len(inPeriod)+1)
	if anchor != nil {
		walk = append(walk, *anchor)
	}
	walk = append(walk, inPeriod...)

	total := 0.0
	for i := 1; i < len(walk); i++ {
		prev, curr := walk[i-1], walk[i]
		if isStart(prev, curr) {
			total += curr.used
		} else {
			delta := curr.used - prev.used
			if delta < 0 {
				delta = 0
			}
			total += delta
		}
	}
	return &total, nil
}

func (c *Calculator) snapshotAtOrBefore(usedCol string, t time.Time) (*snapshot, error) {
	query := fmt.Sprintf(`
		SELECT observed_at, %s, continuous_with_prev
		FROM quota_snapshots
		WHERE %s IS NOT NULL AND observed_at <= ?
		ORDER BY observed_at DESC
		LIMIT 1
	`, usedCol, usedCol)
	var s snapshot
	var cont sql.NullInt64
	err := c.db.QueryRow(query, store.FormatTime(t)).Scan(&s.observedAt, &s.used, &cont)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot anchor query: %w", err)
	}
	s.continuousWithPrev = nullBoolFromInt(cont)
	return &s, nil
}

func (c *Calculator) snapshotsInRange(usedCol string, startTime, endTime time.Time) ([]snapshot, error) {
	query := fmt.Sprintf(`
		SELECT observed_at, %s, continuous_with_prev
		FROM quota_snapshots
		WHERE %s IS NOT NULL AND observed_at > ? AND observed_at <= ?
		ORDER BY observed_at ASC
	`, usedCol, usedCol)
	rows, err := c.db.Query(query, store.FormatTime(startTime), store.FormatTime(endTime))
	if err != nil {
		return nil, fmt.Errorf("snapshot range query: %w", err)
	}
	defer rows.Close()
	var out []snapshot
	for rows.Next() {
		var s snapshot
		var cont sql.NullInt64
		if err := rows.Scan(&s.observedAt, &s.used, &cont); err != nil {
			return nil, err
		}
		s.continuousWithPrev = nullBoolFromInt(cont)
		out = append(out, s)
	}
	return out, rows.Err()
}

// parsePeriod parses a period string like "24h", "7d", "30d". Go's
// time.ParseDuration doesn't accept day units, so a strict "<int>d" form is
// normalized to hours; everything else falls through to time.ParseDuration.
func parsePeriod(periodStr string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(periodStr, "d"); ok {
		if days, err := strconv.Atoi(rest); err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(periodStr)
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %w", err)
	}
	return d, nil
}
