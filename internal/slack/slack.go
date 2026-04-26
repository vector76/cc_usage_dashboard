// Package slack provides the slack (available capacity) signal for queue scheduling.
package slack

import (
	"database/sql"
	"fmt"
	"time"
)

// SlackResponse is the response from GET /slack endpoint.
type SlackResponse struct {
	FiveHourWindow *WindowMetrics `json:"five_hour_window"`
	WeeklyWindow   *WindowMetrics `json:"weekly_window"`
	SlackFraction  *float64       `json:"slack_combined_fraction"` // min of the two, null if either is null
	QuietForSecs   int            `json:"quiet_for_seconds"`
	Paused         bool           `json:"paused"`
	ReleaseRecommended bool        `json:"release_recommended"`
	Gates          map[string]bool `json:"gates"`
}

// WindowMetrics holds the computed metrics for a window.
type WindowMetrics struct {
	Progress      float64 `json:"progress"`       // tokens consumed so far
	Expected      float64 `json:"expected"`       // expected consumption for elapsed time
	Slack         float64 `json:"slack"`          // remaining headroom
	SlackFraction *float64 `json:"slack_fraction"` // (expected - actual) / baseline, nil if window not started
}

// Config holds slack calculation configuration.
type Config struct {
	HeadroomThreshold    float64
	QuietPeriodSeconds   int
	FreshnessThresholdMs int
}

// Calculator computes the slack signal.
type Calculator struct {
	db     *sql.DB
	config Config
	paused bool // in-memory pause state
}

// NewCalculator creates a new slack calculator.
func NewCalculator(db *sql.DB, cfg Config) *Calculator {
	return &Calculator{
		db:     db,
		config: cfg,
		paused: false,
	}
}

// SetPaused sets the pause state.
func (c *Calculator) SetPaused(paused bool) {
	c.paused = paused
}

// GetSlack computes the current slack signal.
func (c *Calculator) GetSlack() (*SlackResponse, error) {
	// Get active windows
	fiveHourWindow, err := c.getActiveWindow("five_hour")
	if err != nil {
		return nil, fmt.Errorf("failed to get 5-hour window: %w", err)
	}

	weeklyWindow, err := c.getActiveWindow("weekly")
	if err != nil {
		return nil, fmt.Errorf("failed to get weekly window: %w", err)
	}

	resp := &SlackResponse{
		Paused: c.paused,
		Gates:  make(map[string]bool),
	}

	// Compute metrics for each window
	if fiveHourWindow != nil {
		metrics, err := c.computeMetrics(fiveHourWindow)
		if err == nil {
			resp.FiveHourWindow = metrics
		}
	}

	if weeklyWindow != nil {
		metrics, err := c.computeMetrics(weeklyWindow)
		if err == nil {
			resp.WeeklyWindow = metrics
		}
	}

	// Combine slack fractions
	resp.SlackFraction = c.combineSlackFractions(resp.FiveHourWindow, resp.WeeklyWindow)

	// Check gates
	quietFor, headroom, fresh := c.checkGates()
	resp.QuietForSecs = int(quietFor.Seconds())

	resp.Gates["headroom"] = headroom
	resp.Gates["priority_quiet"] = quietFor > 0
	resp.Gates["freshness"] = fresh
	resp.Gates["not_paused"] = !c.paused

	// Release is recommended if all gates pass and slack is positive
	resp.ReleaseRecommended = (resp.SlackFraction != nil && *resp.SlackFraction > 0 &&
		headroom && fresh && !c.paused)

	return resp, nil
}

// getActiveWindow fetches the active window of a given kind.
func (c *Calculator) getActiveWindow(kind string) (map[string]interface{}, error) {
	var id, baselineTotal, baselineSource interface{}
	var startedAt, endsAt time.Time

	err := c.db.QueryRow(`
		SELECT id, started_at, ends_at, baseline_total, baseline_source
		FROM windows
		WHERE kind = ? AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`, kind).Scan(&id, &startedAt, &endsAt, &baselineTotal, &baselineSource)

	if err == sql.ErrNoRows {
		return nil, nil // No active window
	} else if err != nil {
		return nil, fmt.Errorf("failed to query window: %w", err)
	}

	return map[string]interface{}{
		"id":              id,
		"started_at":      startedAt,
		"ends_at":         endsAt,
		"baseline_total":  baselineTotal,
		"baseline_source": baselineSource,
	}, nil
}

// computeMetrics computes window metrics from a window record.
func (c *Calculator) computeMetrics(window map[string]interface{}) (*WindowMetrics, error) {
	startedAt := window["started_at"].(time.Time)
	endsAt := window["ends_at"].(time.Time)
	baselineTotal := window["baseline_total"]

	// Get actual consumption in the window
	var actualCost float64
	err := c.db.QueryRow(`
		SELECT COALESCE(SUM(cost_usd_equivalent), 0)
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at < ? AND cost_usd_equivalent IS NOT NULL
	`, startedAt, endsAt).Scan(&actualCost)

	if err != nil {
		return nil, fmt.Errorf("failed to compute consumption: %w", err)
	}

	metrics := &WindowMetrics{
		Progress: actualCost,
	}

	// Compute expected consumption based on elapsed time
	now := time.Now()
	if now.Before(startedAt) {
		// Window hasn't started yet
		metrics.SlackFraction = nil
		return metrics, nil
	}

	windowDuration := endsAt.Sub(startedAt)
	elapsedDuration := now.Sub(startedAt)
	if elapsedDuration < 0 {
		elapsedDuration = 0
	}

	elapsedFraction := float64(elapsedDuration) / float64(windowDuration)

	if baselineTotal != nil {
		baseline := baselineTotal.(float64)
		metrics.Expected = baseline * elapsedFraction
		metrics.Slack = baseline - actualCost

		// Compute slack fraction
		slackFraction := (metrics.Expected - actualCost) / baseline
		metrics.SlackFraction = &slackFraction
	}

	return metrics, nil
}

// combineSlackFractions combines slack fractions from both windows (minimum).
func (c *Calculator) combineSlackFractions(fiveHour, weekly *WindowMetrics) *float64 {
	if fiveHour == nil || fiveHour.SlackFraction == nil {
		if weekly == nil || weekly.SlackFraction == nil {
			return nil
		}
		return weekly.SlackFraction
	}

	if weekly == nil || weekly.SlackFraction == nil {
		return fiveHour.SlackFraction
	}

	// Return the minimum
	min := *fiveHour.SlackFraction
	if *weekly.SlackFraction < min {
		min = *weekly.SlackFraction
	}
	return &min
}

// checkGates checks the three gates: headroom, quiet period, freshness.
// Returns: (time since last event, headroom ok, freshness ok).
func (c *Calculator) checkGates() (time.Duration, bool, bool) {
	now := time.Now()

	// Get time of last event
	var lastEventTime time.Time
	err := c.db.QueryRow(`
		SELECT MAX(occurred_at) FROM usage_events
	`).Scan(&lastEventTime)

	var quietFor time.Duration
	if err == nil && !lastEventTime.IsZero() {
		quietFor = now.Sub(lastEventTime)
	}

	// Get time of last snapshot
	var lastSnapshot time.Time
	err = c.db.QueryRow(`
		SELECT MAX(received_at) FROM quota_snapshots
	`).Scan(&lastSnapshot)

	freshOk := true
	if err == nil && !lastSnapshot.IsZero() {
		age := now.Sub(lastSnapshot)
		threshold := time.Duration(c.config.FreshnessThresholdMs) * time.Millisecond
		freshOk = age < threshold
	}

	headroomOk := true // Placeholder for headroom check

	return quietFor, headroomOk, freshOk
}

// RecordRelease records a release event to the database.
func (c *Calculator) RecordRelease(releasedAt time.Time, jobTag string, estimatedCost *float64, slackAtRelease *float64) (int64, error) {
	// Find which window this release falls into
	var fiveHourWindowID int64
	err := c.db.QueryRow(`
		SELECT id FROM windows
		WHERE kind = 'five_hour' AND started_at <= ? AND ends_at > ?
		LIMIT 1
	`, releasedAt, releasedAt).Scan(&fiveHourWindowID)

	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no active 5-hour window for this release")
	} else if err != nil {
		return 0, fmt.Errorf("failed to find window: %w", err)
	}

	result, err := c.db.Exec(`
		INSERT INTO slack_releases (released_at, received_at, job_tag, estimated_cost, slack_at_release, window_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, releasedAt, time.Now(), jobTag, estimatedCost, slackAtRelease, fiveHourWindowID)

	if err != nil {
		return 0, fmt.Errorf("failed to insert release: %w", err)
	}

	return result.LastInsertId()
}

// RecordSample records a slack sample if sampling is enabled.
func (c *Calculator) RecordSample(fraction *float64) (int64, error) {
	// Find the active 5-hour window
	var windowID int64
	err := c.db.QueryRow(`
		SELECT id FROM windows
		WHERE kind = 'five_hour' AND closed = 0
		LIMIT 1
	`).Scan(&windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to find active window: %w", err)
	}

	result, err := c.db.Exec(`
		INSERT INTO slack_samples (sampled_at, slack_fraction, window_id)
		VALUES (?, ?, ?)
	`, time.Now(), fraction, windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to insert sample: %w", err)
	}

	return result.LastInsertId()
}
