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
		WHERE occurred_at >= ? AND occurred_at < ?
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

// snapshot is one observation of a window's percent-used value, paired with
// the persisted continuity flag (true = continuation of the prior snapshot,
// false/NULL = start / unsafe to delta against the previous row).
type snapshot struct {
	observedAt         time.Time
	used               float64 // 0-100
	continuousWithPrev bool    // false when the source row's flag was NULL or 0
}

// percentConsumed walks the snapshots for the requested window kind and
// sums the per-snapshot increases in `*_used`. Between two adjacent
// snapshots, a continuation contributes the non-negative delta; a start
// (continuous_with_prev = false / NULL) contributes its raw `*_used` as a
// fresh window's worth. The unobserved tail of the prior window — between
// its last snapshot and the reset — is treated as zero; snapshots typically
// arrive right up to window end, so any missed tail is small.
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
	if kind == "weekly" {
		usedCol = "weekly_used"
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
		if curr.continuousWithPrev {
			delta := curr.used - prev.used
			if delta < 0 {
				delta = 0
			}
			total += delta
		} else {
			total += curr.used
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
	s.continuousWithPrev = cont.Valid && cont.Int64 != 0
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
		s.continuousWithPrev = cont.Valid && cont.Int64 != 0
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
