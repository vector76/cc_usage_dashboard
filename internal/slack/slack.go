// Package slack provides the slack (available capacity) signal for queue scheduling.
package slack

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// SlackResponse is the response from GET /slack endpoint. JSON keys match
// docs/slack-indicator.md.
type SlackResponse struct {
	Now                     time.Time       `json:"now"`
	Session                *WindowMetrics  `json:"session"`
	Weekly                  *WindowMetrics  `json:"weekly"`
	SlackCombinedFraction   *float64        `json:"slack_combined_fraction"`
	PriorityQuietForSeconds int             `json:"priority_quiet_for_seconds"`
	Paused                  bool            `json:"paused"`
	ReleaseRecommended      bool            `json:"release_recommended"`
	Gates                   map[string]bool `json:"gates"`
}

// WindowMetrics holds the computed metrics for a single window.
//
// All percent fields are in 0–100 (not 0–1). SlackFraction is in
// [-1, +1] and represents (PercentExpected − PercentUsed) / 100, i.e.
// the fraction of the *full* quota currently held in surplus relative to
// uniform pace. Positive = under pace; negative = over pace.
//
// PercentUsed and SlackFraction are nil whenever no in-window snapshot
// has arrived yet — we don't synthesize an "assumed 0% used" value.
// Consumers (the headroom gates, dashboards) should treat nil as
// "couldn't measure" and fail safe rather than infer.
type WindowMetrics struct {
	WindowStart     time.Time `json:"window_start"`
	WindowEnd       time.Time `json:"window_end"`
	PercentUsed     *float64  `json:"percent_used"`
	PercentExpected float64   `json:"percent_expected"`
	SlackFraction   *float64  `json:"slack_fraction"`
}

// Config holds slack calculation configuration.
type Config struct {
	QuietPeriodSeconds      int
	BaselineMaxAgeHours     int
	SessionSurplusThreshold float64
	WeeklySurplusThreshold  float64
}

// Calculator computes the slack signal. It is safe for concurrent use; a
// single instance is shared across HTTP handlers so the in-memory pause flag
// persists between requests.
type Calculator struct {
	db     *sql.DB
	config Config

	mu     sync.RWMutex
	paused bool
}

// NewCalculator creates a new slack calculator.
func NewCalculator(db *sql.DB, cfg Config) *Calculator {
	return &Calculator{
		db:     db,
		config: cfg,
	}
}

// SetPaused sets the pause state.
func (c *Calculator) SetPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	c.mu.Unlock()
}

// IsPaused reports the current pause state.
func (c *Calculator) IsPaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

// GetSlack computes the current slack signal.
func (c *Calculator) GetSlack() (*SlackResponse, error) {
	now := time.Now().UTC()

	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	sessionWindow, err := c.getActiveWindow("session")
	if err != nil {
		return nil, fmt.Errorf("failed to get 5-hour window: %w", err)
	}

	weeklyWindow, err := c.getActiveWindow("weekly")
	if err != nil {
		return nil, fmt.Errorf("failed to get weekly window: %w", err)
	}

	resp := &SlackResponse{
		Now:    now,
		Paused: paused,
		Gates:  make(map[string]bool),
	}

	if sessionWindow != nil {
		metrics, err := c.computeMetrics(sessionWindow, now)
		if err != nil {
			return nil, fmt.Errorf("failed to compute session metrics: %w", err)
		}
		resp.Session = metrics
	}

	if weeklyWindow != nil {
		metrics, err := c.computeMetrics(weeklyWindow, now)
		if err != nil {
			return nil, fmt.Errorf("failed to compute weekly metrics: %w", err)
		}
		resp.Weekly = metrics
	}

	resp.SlackCombinedFraction = c.combineSlackFractions(resp.Session, resp.Weekly)

	quietFor, hasEvent, err := c.quietFor(now)
	if err != nil {
		return nil, err
	}
	resp.PriorityQuietForSeconds = int(quietFor.Seconds())

	quietThreshold := time.Duration(c.config.QuietPeriodSeconds) * time.Second
	priorityQuietOk := !hasEvent || quietFor >= quietThreshold

	sessionHeadroomOk := resp.Session != nil &&
		resp.Session.SlackFraction != nil &&
		*resp.Session.SlackFraction >= c.config.SessionSurplusThreshold
	weeklyHeadroomOk := resp.Weekly != nil &&
		resp.Weekly.SlackFraction != nil &&
		*resp.Weekly.SlackFraction >= c.config.WeeklySurplusThreshold

	freshOk, err := c.baselineFreshnessOk(now)
	if err != nil {
		return nil, err
	}

	resp.Gates["session_headroom"] = sessionHeadroomOk
	resp.Gates["weekly_headroom"] = weeklyHeadroomOk
	resp.Gates["priority_quiet"] = priorityQuietOk
	resp.Gates["baseline_freshness"] = freshOk
	resp.Gates["not_paused"] = !paused

	resp.ReleaseRecommended = sessionHeadroomOk && weeklyHeadroomOk && priorityQuietOk && freshOk && !paused

	return resp, nil
}

// activeWindow holds the fields we need from the windows table.
type activeWindow struct {
	startedAt     time.Time
	endsAt        time.Time
	baselineTotal *float64
}

// getActiveWindow fetches the active window of a given kind.
func (c *Calculator) getActiveWindow(kind string) (*activeWindow, error) {
	var w activeWindow
	var baselineTotal sql.NullFloat64

	err := c.db.QueryRow(`
		SELECT started_at, ends_at, baseline_total
		FROM windows
		WHERE kind = ? AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`, kind).Scan(&w.startedAt, &w.endsAt, &baselineTotal)

	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to query window: %w", err)
	}

	if baselineTotal.Valid {
		v := baselineTotal.Float64
		w.baselineTotal = &v
	}
	return &w, nil
}

// computeMetrics computes window metrics for an active window using
// percent-of-quota math only. PercentUsed comes from the latest in-window
// snapshot (windows.baseline_total, which the windows engine keeps current);
// dollar consumption from usage_events does not enter the slack signal.
//
//	progress(t)        = clamp((t - t0) / (t1 - t0), 0, 1)
//	percent_expected   = 100 * progress(t)              # uniform pace to 100% by t1
//	slack_fraction     = (percent_expected - percent_used) / 100   # in [-1, +1]
//
// percent_used and slack_fraction are nil when no in-window snapshot has
// been recorded yet — we fail safe rather than assume 0% used.
func (c *Calculator) computeMetrics(w *activeWindow, now time.Time) (*WindowMetrics, error) {
	m := &WindowMetrics{
		WindowStart: w.startedAt,
		WindowEnd:   w.endsAt,
	}
	if w.baselineTotal != nil {
		v := *w.baselineTotal
		m.PercentUsed = &v
	}

	// Window has not started yet.
	if now.Before(w.startedAt) {
		return m, nil
	}

	windowDuration := w.endsAt.Sub(w.startedAt)
	if windowDuration <= 0 {
		return m, nil
	}
	elapsed := now.Sub(w.startedAt)
	progress := float64(elapsed) / float64(windowDuration)
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	m.PercentExpected = 100 * progress

	if m.PercentUsed != nil {
		frac := (m.PercentExpected - *m.PercentUsed) / 100
		m.SlackFraction = &frac
	}

	return m, nil
}

// combineSlackFractions returns min(a, b) of the two windows' slack fractions.
// Per docs: combined is null whenever either window's slack_fraction is null.
func (c *Calculator) combineSlackFractions(session, weekly *WindowMetrics) *float64 {
	if session == nil || session.SlackFraction == nil {
		return nil
	}
	if weekly == nil || weekly.SlackFraction == nil {
		return nil
	}
	combined := min(*session.SlackFraction, *weekly.SlackFraction)
	return &combined
}

// quietFor returns the time since the most recent usage event and a flag
// indicating whether any events exist.
//
// We use ORDER BY ... LIMIT 1 instead of MAX() because go-sqlite3 erases the
// column type for aggregate results, returning the timestamp as a raw string
// that does not scan into time.Time.
func (c *Calculator) quietFor(now time.Time) (time.Duration, bool, error) {
	var lastEvent time.Time
	err := c.db.QueryRow(`SELECT occurred_at FROM usage_events ORDER BY occurred_at DESC LIMIT 1`).Scan(&lastEvent)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to query last event: %w", err)
	}
	if lastEvent.IsZero() {
		return 0, false, nil
	}
	dt := now.Sub(lastEvent)
	if dt < 0 {
		dt = 0
	}
	return dt, true, nil
}

// baselineFreshnessOk implements the freshness gate from
// docs/slack-indicator.md: the gate passes iff a snapshot exists and is no
// older than BaselineMaxAgeHours. Missing snapshot fails the gate.
func (c *Calculator) baselineFreshnessOk(now time.Time) (bool, error) {
	var receivedAt time.Time
	err := c.db.QueryRow(`
		SELECT received_at FROM quota_snapshots
		ORDER BY received_at DESC LIMIT 1
	`).Scan(&receivedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to query snapshot: %w", err)
	}

	age := now.Sub(receivedAt)
	maxAge := time.Duration(c.config.BaselineMaxAgeHours) * time.Hour
	return age <= maxAge, nil
}

// RecordRelease records a release event to the database, resolving it to the
// active window of the requested kind containing releasedAt.
func (c *Calculator) RecordRelease(releasedAt time.Time, jobTag string, estimatedCost *float64, slackAtRelease *float64, windowKind string) (int64, error) {
	if windowKind == "" {
		windowKind = "session"
	}

	var windowID int64
	err := c.db.QueryRow(`
		SELECT id FROM windows
		WHERE kind = ? AND started_at <= ? AND ends_at > ?
		ORDER BY started_at DESC
		LIMIT 1
	`, windowKind, store.FormatTime(releasedAt), store.FormatTime(releasedAt)).Scan(&windowID)

	if err == sql.ErrNoRows {
		return 0, ErrNoActiveWindow
	} else if err != nil {
		return 0, fmt.Errorf("failed to find window: %w", err)
	}

	result, err := c.db.Exec(`
		INSERT INTO slack_releases (released_at, received_at, job_tag, estimated_cost, slack_at_release, window_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, store.FormatTime(releasedAt), store.FormatTime(time.Now()), jobTag, estimatedCost, slackAtRelease, windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to insert release: %w", err)
	}

	return result.LastInsertId()
}

// ErrNoActiveWindow is returned by RecordRelease when no window of the
// requested kind contains the releasedAt timestamp.
var ErrNoActiveWindow = fmt.Errorf("no active window")

// RecordSample records a slack sample if sampling is enabled.
func (c *Calculator) RecordSample(fraction *float64) (int64, error) {
	var windowID int64
	err := c.db.QueryRow(`
		SELECT id FROM windows
		WHERE kind = 'session' AND closed = 0
		LIMIT 1
	`).Scan(&windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to find active window: %w", err)
	}

	result, err := c.db.Exec(`
		INSERT INTO slack_samples (sampled_at, slack_fraction, window_id)
		VALUES (?, ?, ?)
	`, store.FormatTime(time.Now()), fraction, windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to insert sample: %w", err)
	}

	return result.LastInsertId()
}
