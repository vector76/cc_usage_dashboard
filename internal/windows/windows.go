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
	BaselinePercentUsed *float64 // Percent-used anchor (0–100) from the most-recent in-window snapshot.
	BaselineSource      string
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
			UPDATE windows SET baseline_percent_used = ?, baseline_source = ?
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
		SELECT id, started_at, ends_at, baseline_percent_used, baseline_source
		FROM windows
		WHERE kind = 'session' AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&window.ID, &window.StartedAt, &window.EndsAt, &window.BaselinePercentUsed, &window.BaselineSource)

	if err == nil {
		// Window exists. If it's still active, optionally re-anchor it on
		// the snapshot's authoritative end. ensureSessionWindow used to
		// only consult snapshots at creation time, which left active
		// windows stuck on whatever boundary they were born with even
		// after the userscript started reporting Anthropic's actual
		// reset. We re-anchor when the snapshot end differs by more than
		// the rounding tolerance below.
		if window.EndsAt.After(now) {
			// Early-close path: if Anthropic's UI now reports the session
			// as inactive AND no usage in the current session window, the
			// window is over even though our calendar/snapshot boundary
			// hasn't elapsed yet. Close on the snapshot's observed_at so
			// downstream event-anchored opening can distinguish post-
			// closure events from in-window events.
			active, used, observedAt, err := e.findMostRecentSessionActive()
			if err != nil {
				return err
			}
			// Defensive contradiction: if the snapshot says inactive but
			// reports nonzero usage, leave the window alone — Anthropic
			// briefly flickers session_active=false while a session is
			// opening.
			if active != nil && !*active && used != nil && *used == 0 {
				if _, err := e.db.Exec(
					`UPDATE windows SET closed = 1, ends_at = ? WHERE id = ?`,
					store.FormatTime(observedAt), window.ID,
				); err != nil {
					return fmt.Errorf("failed to close window early: %w", err)
				}
				return nil
			}
			if err := e.reanchorIfStale(window.ID, window.EndsAt, "session", 5*time.Hour); err != nil {
				return err
			}
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

	// If the most recent snapshot reports the session as inactive, do not
	// mint a phantom replacement window. Anthropic considers there to be no
	// active session, and zero open session rows is a permitted state.
	active, _, _, err := e.findMostRecentSessionActive()
	if err != nil {
		return err
	}
	if active != nil && !*active {
		return nil
	}

	// Prefer the snapshot's authoritative reset time ("Resets in N hr M min"
	// parsed by the userscript). When no future snapshot boundary is
	// available, fall back to usage-event evidence: open a window only if a
	// usage_event exists that postdates the most recent closed session
	// window's ends_at (or no closed session window exists at all). This
	// covers both natural expiration and early-closure (where ends_at is
	// snapshot.observed_at) without minting phantom windows on snapshot-only
	// activity.
	var startTime, endsAt time.Time
	if t, err := e.findSessionBoundary(); err == nil && !t.IsZero() && t.After(now) {
		endsAt = t
		startTime = endsAt.Add(-5 * time.Hour)
	} else {
		eventTime, lastClosedEnds, err := e.findEventEvidenceForOpen()
		if err != nil {
			return err
		}
		if eventTime.IsZero() {
			return nil
		}
		if !lastClosedEnds.IsZero() && !eventTime.After(lastClosedEnds) {
			return nil
		}
		startTime = eventTime
		endsAt = eventTime.Add(5 * time.Hour)
	}

	// Get baseline from most recent session snapshot at or before window start
	baseline, baselineSource, err := e.findBaseline("session", startTime)
	if err != nil {
		slog.Error("failed to find baseline", "err", err)
		baseline = nil
		baselineSource = "default"
	}

	// Insert new window
	_, err = e.db.Exec(`
		INSERT INTO windows (kind, started_at, ends_at, baseline_percent_used, baseline_source, closed)
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
		SELECT id, started_at, ends_at, baseline_percent_used, baseline_source
		FROM windows
		WHERE kind = 'weekly' AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&window.ID, &window.StartedAt, &window.EndsAt, &window.BaselinePercentUsed, &window.BaselineSource)

	if err == nil {
		// Window exists. Re-anchor on snapshot boundary if needed (see
		// comment in ensureSessionWindow); same reason — a window born
		// from one snapshot's boundary can drift if a later snapshot
		// reports a more precise or corrected reset time, and the
		// active window should follow rather than wait up to 7 days
		// for natural expiry.
		if window.EndsAt.After(now) {
			if err := e.reanchorIfStale(window.ID, window.EndsAt, "weekly", 7*24*time.Hour); err != nil {
				return err
			}
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

	// Mint a new weekly window iff a snapshot supplies an authoritative
	// boundary (weekly_window_ends) that is strictly in the future. The
	// After(now) check guards against minting on a just-passed boundary,
	// which would produce a born-expired row that the next tick closes
	// and re-mints — a loop that floods the windows table with zombie
	// rows.
	//
	// When no usable boundary is available — userscript reports limbo
	// (weekly_active=false), the DB is empty, an older userscript never
	// emitted weekly_window_ends, or a stale boundary lingers across a
	// rollover — refuse to mint. The dashboard then synthesizes a
	// hypothetical [now, now+7d] (see synthesizeHypotheticalWeekly), so
	// the chart always projects from the current time rather than from
	// a calendar-aligned guess. This mirrors the session path, which
	// also mints nothing in the absence of authoritative evidence. See
	// docs/no-active-session.md.
	endsAt, err := e.findWeeklyBoundary()
	if err != nil {
		return err
	}
	if endsAt.IsZero() || !endsAt.After(now) {
		return nil
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
		INSERT INTO windows (kind, started_at, ends_at, baseline_percent_used, baseline_source, closed)
		VALUES (?, ?, ?, ?, ?, 0)
	`, "weekly", store.FormatTime(startTime), store.FormatTime(endsAt), baseline, baselineSource)

	if err != nil {
		return fmt.Errorf("failed to insert window: %w", err)
	}

	return nil
}

// findEventEvidenceForOpen returns the timestamp of the most recent usage_event
// (zero if none) and the ends_at of the most recent closed session window
// (zero if none). The caller decides whether to open a new session window
// based on the event-evidence rule: open iff an event exists AND either no
// closed session window exists or the event is strictly newer than the most
// recent closed window's ends_at.
func (e *Engine) findEventEvidenceForOpen() (time.Time, time.Time, error) {
	var eventTime time.Time
	err := e.db.QueryRow(
		`SELECT occurred_at FROM usage_events ORDER BY occurred_at DESC LIMIT 1`,
	).Scan(&eventTime)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to query latest usage event: %w", err)
	}

	var lastClosedEnds time.Time
	err = e.db.QueryRow(
		`SELECT ends_at FROM windows WHERE kind = 'session' AND closed = 1 ORDER BY ends_at DESC LIMIT 1`,
	).Scan(&lastClosedEnds)
	if err != nil && err != sql.ErrNoRows {
		return time.Time{}, time.Time{}, fmt.Errorf("failed to query last closed session window: %w", err)
	}

	return eventTime, lastClosedEnds, nil
}

// findBaseline finds the baseline (latest known % used) for the given window
// kind at or before t. Used to seed a new window's baseline_percent_used when no
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

// reanchorIfStale updates an active window's started_at/ends_at to match the
// most recent snapshot's window_ends, when that boundary differs from the
// current ends_at by more than the rounding tolerance.
//
// Why: snapshot-supplied boundaries ("Resets in 4 hr 55 min") have minute
// resolution; the same instant rendered into the page can drift ±1 min as
// the user lingers. We tolerate up to 2 min of drift to avoid thrashing
// while still re-anchoring when a later snapshot reports a materially
// different reset time than the one the window was born with (e.g. a
// coarse "Resets May 1" hint replaced by a precise "Resets in 23 hr 14
// min").
//
// Refuse to re-anchor onto a snapshot boundary that's already in the past:
// findWeeklyBoundary (and findSessionBoundary) return the most recent
// non-null boundary regardless of staleness, so right after a reset the
// returned timestamp is the just-passed boundary. Re-anchoring an active
// window onto that would push its ends_at into the past, immediately
// expiring it and triggering the same close-and-mint loop the
// ensureWeeklyWindow After(now) guard prevents on the open path.
func (e *Engine) reanchorIfStale(windowID int64, currentEndsAt time.Time, kind string, duration time.Duration) error {
	const tolerance = 2 * time.Minute

	var snapshotEnds time.Time
	var err error
	switch kind {
	case "session":
		snapshotEnds, err = e.findSessionBoundary()
	case "weekly":
		snapshotEnds, err = e.findWeeklyBoundary()
	default:
		return nil
	}
	if err != nil || snapshotEnds.IsZero() || !snapshotEnds.After(e.now()) {
		return nil
	}

	delta := snapshotEnds.Sub(currentEndsAt)
	if delta < 0 {
		delta = -delta
	}
	if delta < tolerance {
		return nil
	}

	newStart := snapshotEnds.Add(-duration)
	if _, err := e.db.Exec(
		`UPDATE windows SET started_at = ?, ends_at = ? WHERE id = ?`,
		store.FormatTime(newStart), store.FormatTime(snapshotEnds), windowID,
	); err != nil {
		return fmt.Errorf("failed to re-anchor window %d: %w", windowID, err)
	}
	slog.Info("re-anchored window to snapshot boundary",
		"id", windowID, "kind", kind, "old_ends", currentEndsAt, "new_ends", snapshotEnds)
	return nil
}

// findSessionBoundary extracts the session reset time from the most recent
// snapshot that supplied one. Userscript v0.3+ parses "Resets in N hr M min"
// and sends it as session_window_ends; older snapshots may have NULL.
func (e *Engine) findSessionBoundary() (time.Time, error) {
	var boundary time.Time
	err := e.db.QueryRow(`
		SELECT session_window_ends
		FROM quota_snapshots
		WHERE session_window_ends IS NOT NULL
		ORDER BY observed_at DESC
		LIMIT 1
	`).Scan(&boundary)

	if err == sql.ErrNoRows {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, fmt.Errorf("failed to query session boundary: %w", err)
	}

	return boundary, nil
}

// findMostRecentSessionActive reads session_active, session_used, and
// observed_at from the same most-recent quota_snapshots row (by observed_at).
// Both columns are guaranteed to come from the SAME row so callers can
// reason about contradictions (e.g. session_active=false but session_used>0).
// Returns nil pointers for columns that are NULL; observedAt is zero when no
// snapshots exist.
func (e *Engine) findMostRecentSessionActive() (*bool, *float64, time.Time, error) {
	var active sql.NullInt64
	var used sql.NullFloat64
	var observedAt time.Time
	err := e.db.QueryRow(`
		SELECT session_active, session_used, observed_at
		FROM quota_snapshots
		ORDER BY observed_at DESC
		LIMIT 1
	`).Scan(&active, &used, &observedAt)

	if err == sql.ErrNoRows {
		return nil, nil, time.Time{}, nil
	} else if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("failed to query session snapshot: %w", err)
	}

	var activePtr *bool
	if active.Valid {
		v := active.Int64 != 0
		activePtr = &v
	}
	var usedPtr *float64
	if used.Valid {
		usedPtr = &used.Float64
	}
	return activePtr, usedPtr, observedAt, nil
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

