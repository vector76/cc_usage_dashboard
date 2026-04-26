// Package windows provides window derivation and baseline management.
package windows

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// Window represents a session or weekly quota window.
type Window struct {
	ID             int64
	Kind           string // "session" or "weekly"
	StartedAt      time.Time
	EndsAt         time.Time
	BaselineTotal  *float64 // Percent-used anchor (0–100) from the most-recent in-window snapshot.
	BaselineSource string
	Closed         bool
}

// Engine maintains the windows table and derives baseline quotas.
type Engine struct {
	db *sql.DB
	now func() time.Time // Allows injection of time for testing
}

// NewEngine creates a new windows engine.
func NewEngine(db *sql.DB) *Engine {
	return &Engine{
		db:  db,
		now: time.Now,
	}
}

// SetNow sets the time function (for testing).
func (e *Engine) SetNow(fn func() time.Time) {
	e.now = fn
}

// UpdateWindows updates the windows table after an event or snapshot.
// This should be called after inserting usage events or snapshots.
func (e *Engine) UpdateWindows() error {
	// Get or create the session window
	if err := e.ensureSessionWindow(); err != nil {
		return err
	}

	// Get or create the weekly window
	if err := e.ensureWeeklyWindow(); err != nil {
		return err
	}

	// Correct baselines for any in-window snapshots
	if err := e.correctBaselineFromSnapshots(); err != nil {
		return err
	}

	return nil
}

// correctBaselineFromSnapshots updates window baselines when snapshots occur within the window.
func (e *Engine) correctBaselineFromSnapshots() error {
	rows, err := e.db.Query(`
		SELECT id, kind, started_at, ends_at FROM windows WHERE closed = 0
	`)
	if err != nil {
		return fmt.Errorf("failed to query windows: %w", err)
	}
	defer rows.Close()

	type activeWindow struct {
		id        int64
		kind      string
		startedAt time.Time
		endsAt    time.Time
	}
	var active []activeWindow
	for rows.Next() {
		var w activeWindow
		if err := rows.Scan(&w.id, &w.kind, &w.startedAt, &w.endsAt); err != nil {
			return fmt.Errorf("failed to scan window: %w", err)
		}
		active = append(active, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, w := range active {
		var usedCol string
		switch w.kind {
		case "session":
			usedCol = "session_used"
		case "weekly":
			usedCol = "weekly_used"
		default:
			continue
		}

		var snapshotID int64
		var baseline float64
		query := fmt.Sprintf(`
			SELECT id, %s FROM quota_snapshots
			WHERE observed_at >= ? AND observed_at < ? AND %s IS NOT NULL
			ORDER BY observed_at DESC
			LIMIT 1
		`, usedCol, usedCol)
		err := e.db.QueryRow(query, store.FormatTime(w.startedAt), store.FormatTime(w.endsAt)).Scan(&snapshotID, &baseline)

		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to query snapshot for window %d: %w", w.id, err)
		}

		baselineSource := fmt.Sprintf("snapshot:%d", snapshotID)
		slog.Debug("correcting baseline", "windowID", w.id, "newBaseline", baseline)
		if _, err := e.db.Exec(`
			UPDATE windows SET baseline_total = ?, baseline_source = ?
			WHERE id = ?
		`, baseline, baselineSource, w.id); err != nil {
			return fmt.Errorf("failed to update window baseline: %w", err)
		}
	}

	return nil
}

// ensureSessionWindow ensures a session (5-hour) window exists for the current period.
func (e *Engine) ensureSessionWindow() error {
	now := e.now()

	// Get the most recent active session window
	var window Window
	err := e.db.QueryRow(`
		SELECT id, started_at, ends_at, baseline_total, baseline_source
		FROM windows
		WHERE kind = 'session' AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&window.ID, &window.StartedAt, &window.EndsAt, &window.BaselineTotal, &window.BaselineSource)

	if err == nil {
		// Window exists, check if it's still valid
		if window.EndsAt.After(now) {
			// Current window is still active
			return nil
		}

		// Window has expired, close it
		_, err := e.db.Exec("UPDATE windows SET closed = 1 WHERE id = ?", window.ID)
		if err != nil {
			return fmt.Errorf("failed to close window: %w", err)
		}
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("failed to query windows: %w", err)
	}

	// Create a new window starting from first event after gap
	// For now, use "first event" detection logic
	startTime, err := e.findFirstEventAfterGap("session")
	if err != nil || startTime.IsZero() {
		// No events yet, start from now
		startTime = now
	}

	endsAt := startTime.Add(5 * time.Hour)

	// Get baseline from most recent session snapshot at or before window start
	baseline, baselineSource, err := e.findBaseline("session", startTime)
	if err != nil {
		slog.Error("failed to find baseline", "err", err)
		baseline = nil
		baselineSource = "default"
	}

	// Insert new window
	_, err = e.db.Exec(`
		INSERT INTO windows (kind, started_at, ends_at, baseline_total, baseline_source, closed)
		VALUES (?, ?, ?, ?, ?, 0)
	`, "session", store.FormatTime(startTime), store.FormatTime(endsAt), baseline, baselineSource)

	if err != nil {
		return fmt.Errorf("failed to insert window: %w", err)
	}

	return nil
}

// ensureWeeklyWindow ensures a weekly window exists for the current period.
func (e *Engine) ensureWeeklyWindow() error {
	now := e.now()

	// Get the most recent active weekly window
	var window Window
	err := e.db.QueryRow(`
		SELECT id, started_at, ends_at, baseline_total, baseline_source
		FROM windows
		WHERE kind = 'weekly' AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&window.ID, &window.StartedAt, &window.EndsAt, &window.BaselineTotal, &window.BaselineSource)

	if err == nil {
		// Window exists, check if it's still valid
		if window.EndsAt.After(now) {
			return nil
		}

		// Window has expired, close it
		_, err := e.db.Exec("UPDATE windows SET closed = 1 WHERE id = ?", window.ID)
		if err != nil {
			return fmt.Errorf("failed to close window: %w", err)
		}
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("failed to query windows: %w", err)
	}

	// Try to get window boundary from the most recent snapshot
	endsAt, err := e.findWeeklyBoundary()
	if err != nil || endsAt.IsZero() {
		// Default: midnight UTC at the start of the upcoming Monday
		// (i.e. end-of-Sunday boundary). Must be in the future relative
		// to `now` or the window is born already-expired.
		endsAt = e.nextMondayMidnight(now)
	}

	startTime := endsAt.Add(-7 * 24 * time.Hour)

	// Get baseline from most recent weekly snapshot at or before window start
	baseline, baselineSource, err := e.findBaseline("weekly", startTime)
	if err != nil {
		slog.Error("failed to find baseline", "err", err)
		baseline = nil
		baselineSource = "default"
	}

	// Insert new window
	_, err = e.db.Exec(`
		INSERT INTO windows (kind, started_at, ends_at, baseline_total, baseline_source, closed)
		VALUES (?, ?, ?, ?, ?, 0)
	`, "weekly", store.FormatTime(startTime), store.FormatTime(endsAt), baseline, baselineSource)

	if err != nil {
		return fmt.Errorf("failed to insert window: %w", err)
	}

	return nil
}

// findFirstEventAfterGap finds the timestamp of the first event after a gap.
// Returns zero time if no events exist.
func (e *Engine) findFirstEventAfterGap(windowKind string) (time.Time, error) {
	// Get the most recent closed window
	var lastEnds time.Time
	err := e.db.QueryRow(`
		SELECT MAX(ends_at) FROM windows
		WHERE kind = ? AND closed = 1
	`, windowKind).Scan(&lastEnds)

	if err != nil || lastEnds.IsZero() {
		// No closed windows, return zero time
		return time.Time{}, nil
	}

	// Get the first event after the window ended
	var firstEvent time.Time
	err = e.db.QueryRow(`
		SELECT MIN(occurred_at) FROM usage_events
		WHERE occurred_at > ?
	`, store.FormatTime(lastEnds)).Scan(&firstEvent)

	if err != nil || firstEvent.IsZero() {
		return time.Time{}, nil
	}

	return firstEvent, nil
}

// findBaseline finds the baseline (latest known % used) for the given window
// kind at or before t. Used to seed a new window's baseline_total when no
// in-window snapshot exists yet. Subsequent in-window snapshots refine the
// value via correctBaselineFromSnapshots.
func (e *Engine) findBaseline(kind string, t time.Time) (*float64, string, error) {
	var usedCol string
	switch kind {
	case "session":
		usedCol = "session_used"
	case "weekly":
		usedCol = "weekly_used"
	default:
		return nil, "no_snapshot", nil
	}

	var baselineTotal sql.NullFloat64
	query := fmt.Sprintf(`
		SELECT %s
		FROM quota_snapshots
		WHERE observed_at <= ? AND %s IS NOT NULL
		ORDER BY observed_at DESC
		LIMIT 1
	`, usedCol, usedCol)
	err := e.db.QueryRow(query, store.FormatTime(t)).Scan(&baselineTotal)

	if err == sql.ErrNoRows {
		// No snapshot found, use default
		return nil, "no_snapshot", nil
	} else if err != nil {
		return nil, "", fmt.Errorf("failed to query baseline: %w", err)
	}

	if !baselineTotal.Valid {
		return nil, "no_snapshot", nil
	}

	return &baselineTotal.Float64, "snapshot", nil
}

// findWeeklyBoundary extracts the weekly window boundary from the most recent snapshot.
func (e *Engine) findWeeklyBoundary() (time.Time, error) {
	var boundary time.Time
	err := e.db.QueryRow(`
		SELECT weekly_window_ends
		FROM quota_snapshots
		WHERE weekly_window_ends IS NOT NULL
		ORDER BY observed_at DESC
		LIMIT 1
	`).Scan(&boundary)

	if err == sql.ErrNoRows {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, fmt.Errorf("failed to query boundary: %w", err)
	}

	return boundary, nil
}

// nextMondayMidnight returns the next Monday at midnight UTC.
func (e *Engine) nextMondayMidnight(t time.Time) time.Time {
	// Monday = 1
	for t.Weekday() != time.Monday {
		t = t.Add(24 * time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Drift returns the drift between actual consumption and baseline.
func (e *Engine) Drift(windowID int64) (*float64, error) {
	// Get the window
	var baseline *float64
	var kind string
	err := e.db.QueryRow(`
		SELECT kind, baseline_total FROM windows WHERE id = ?
	`, windowID).Scan(&kind, &baseline)

	if err != nil {
		return nil, fmt.Errorf("failed to query window: %w", err)
	}

	// Get the sum of costs for events in the window
	var totalCost float64
	err = e.db.QueryRow(`
		SELECT COALESCE(SUM(cost_usd_equivalent), 0)
		FROM usage_events
		WHERE cost_usd_equivalent IS NOT NULL
		AND occurred_at >= (
			SELECT started_at FROM windows WHERE id = ?
		)
		AND occurred_at < (
			SELECT ends_at FROM windows WHERE id = ?
		)
	`, windowID, windowID).Scan(&totalCost)

	if err != nil {
		return nil, fmt.Errorf("failed to query costs: %w", err)
	}

	if baseline == nil {
		return nil, nil
	}

	drift := *baseline - totalCost
	return &drift, nil
}
