package windows

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func createTestEngine(t *testing.T) (*Engine, *store.Store) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	engine := NewEngine(s.DB())
	return engine, s
}

func TestFirstEventCreatesSessionWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert an event
	_, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	)
	if err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	// Update windows
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Check that a session window was created
	var count int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 session window, got %d", count)
	}

	// Check window times
	var startedAt, endsAt time.Time
	row = s.DB().QueryRow(`SELECT started_at, ends_at FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&startedAt, &endsAt); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	expectedEndsAt := now.Add(5 * time.Hour)
	if !startedAt.Equal(now) {
		t.Errorf("expected started_at %v, got %v", now, startedAt)
	}
	if !endsAt.Equal(expectedEndsAt) {
		t.Errorf("expected ends_at %v, got %v", expectedEndsAt, endsAt)
	}
}

func TestFirstEventCreatesWeeklyWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert an event
	_, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	)
	if err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	// Update windows
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Check that a weekly window was created
	var count int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'weekly' AND closed = 0`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 weekly window, got %d", count)
	}
}

func TestWeeklyWindowEndsFromSnapshot(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	// Wednesday April 22 2026 — verifiable Sunday default is April 26 00:00 UTC.
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Snapshot supplies an explicit weekly window boundary that differs from
	// the default Sunday boundary so we can tell which path was taken.
	weeklyEnds := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	_, err := s.InsertQuotaSnapshot(
		now.Add(-time.Hour), now.Add(-time.Hour), "test",
		nil, nil,
		nil, &weeklyEnds,
		nil,
		nil,
		nil,
		"{}",
	)
	if err != nil {
		t.Fatalf("failed to insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var startedAt, endsAt time.Time
	row := s.DB().QueryRow(`SELECT started_at, ends_at FROM windows WHERE kind = 'weekly' AND closed = 0`)
	if err := row.Scan(&startedAt, &endsAt); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if !endsAt.Equal(weeklyEnds) {
		t.Errorf("weekly ends_at: got %v, want %v (from snapshot)", endsAt, weeklyEnds)
	}
	wantStart := weeklyEnds.Add(-7 * 24 * time.Hour)
	if !startedAt.Equal(wantStart) {
		t.Errorf("weekly started_at: got %v, want %v", startedAt, wantStart)
	}
}

func TestWeeklyWindowDefaultBoundary(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	// Wednesday April 22 2026 12:00 UTC. Default boundary is the midnight
	// at the start of the upcoming Monday: Monday April 27 00:00 UTC. The
	// older formulation subtracted 24h, which put ends_at in the past on a
	// Sunday and caused the freshly-created window to be born expired.
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var endsAt time.Time
	row := s.DB().QueryRow(`SELECT ends_at FROM windows WHERE kind = 'weekly' AND closed = 0`)
	if err := row.Scan(&endsAt); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	wantEnds := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	if !endsAt.Equal(wantEnds) {
		t.Errorf("default weekly ends_at: got %v, want %v", endsAt, wantEnds)
	}

	// Regression guard: the window must be in the future relative to `now`.
	// Without this, a fresh weekly window could be created already-expired,
	// excluding a contemporaneous snapshot from the in-window baseline pass.
	if !endsAt.After(now) {
		t.Errorf("default weekly ends_at must be after now; got %v vs now %v", endsAt, now)
	}
}

// TestWeeklyWindowOnSundayDoesNotExpireImmediately is a regression test for
// the bug where, on a Sunday, the default fallback (Sunday 00:00 UTC of the
// current week) put ends_at in the past, so a snapshot recorded later that
// same Sunday fell outside the synthesized window and never seeded the
// weekly baseline.
func TestWeeklyWindowOnSundayDoesNotExpireImmediately(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	// Sunday April 26 2026 20:12 UTC — the failure mode was reported here.
	now := time.Date(2026, 4, 26, 20, 12, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	weekly := 24.0
	if _, err := s.InsertQuotaSnapshot(
		now, now, "userscript",
		nil, nil,
		&weekly, nil,
		nil,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows: %v", err)
	}

	var startedAt, endsAt time.Time
	var baseline sql.NullFloat64
	row := s.DB().QueryRow(`SELECT started_at, ends_at, baseline_percent_used FROM windows WHERE kind = 'weekly' AND closed = 0`)
	if err := row.Scan(&startedAt, &endsAt, &baseline); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if !endsAt.After(now) {
		t.Fatalf("weekly ends_at=%v is not after now=%v; window is born expired", endsAt, now)
	}
	if !startedAt.Before(now) || !endsAt.After(now) {
		t.Errorf("snapshot must fall inside the weekly window; got [%v, %v) for now=%v", startedAt, endsAt, now)
	}
	if !baseline.Valid || baseline.Float64 != weekly {
		t.Errorf("expected weekly baseline_percent_used=%v, got %v", weekly, baseline)
	}
}

// When the most recent snapshot reports weekly_active=false and supplies no
// weekly_window_ends, the engine must NOT mint a phantom weekly window. This
// is the symmetric guard for the session early-refusal path: the dashboard's
// hypothetical rendering covers the gap.
func TestWeeklyLimboSuppressesPhantomWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	inactive := false
	if _, err := s.InsertQuotaSnapshot(
		now, now, "userscript",
		nil, nil,
		nil, nil, // no weekly_window_ends
		nil,
		&inactive, // weekly_active = false
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows: %v", err)
	}

	var count int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'weekly'`,
	).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("expected zero weekly windows under limbo, got %d", count)
	}
}

// Regression: weekly_active=false must not block opening a window when the
// snapshot also supplies an authoritative weekly_window_ends. The userscript
// won't emit both simultaneously today (limbo replaces the "Resets …" hint),
// but if it ever does, the boundary should win — exactly the same precedence
// as the session path.
func TestWeeklyLimboWithBoundaryStillOpensWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	weeklyEnds := now.Add(48 * time.Hour)
	inactive := false
	if _, err := s.InsertQuotaSnapshot(
		now, now, "userscript",
		nil, nil,
		nil, &weeklyEnds,
		nil,
		&inactive,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows: %v", err)
	}

	var count int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'weekly' AND closed = 0`,
	).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 open weekly window when boundary present, got %d", count)
	}
}

func TestMultipleEventsInSameWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert multiple events
	for i := 0; i < 3; i++ {
		_, err := s.InsertUsageEvent(
			now.Add(time.Duration(i)*time.Minute), "test", "session-1",
			"msg-"+string(rune('1'+i)), "", "claude-3-5-sonnet-20241022",
			100, 50, 0, 0, nil, "", "{}",
		)
		if err != nil {
			t.Fatalf("failed to insert event: %v", err)
		}
	}

	// Update windows
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Should still have only 1 session window
	var count int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 session window, got %d", count)
	}
}

func TestWindowExpiry(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert first event
	_, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	)
	if err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	// Update windows
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Move time forward past the window end (5 hours + 1 minute)
	laterTime := now.Add(5*time.Hour + 1*time.Minute)
	engine.SetNow(func() time.Time { return laterTime })

	// Insert a new event
	_, err = s.InsertUsageEvent(
		laterTime, "test", "session-1", "msg-2", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	)
	if err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	// Update windows
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Check that the old window is closed
	var closedCount int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 1`)
	if err := row.Scan(&closedCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if closedCount != 1 {
		t.Errorf("expected 1 closed window, got %d", closedCount)
	}

	// Check that a new window exists.
	var openCount int
	row = s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&openCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if openCount != 1 {
		t.Errorf("expected 1 open window, got %d", openCount)
	}

	// And that it starts at the post-gap event (the event-evidence rule
	// anchors the new window at the most recent usage_event timestamp).
	var newStart time.Time
	row = s.DB().QueryRow(`SELECT started_at FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&newStart); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if !newStart.Equal(laterTime) {
		t.Errorf("new window started_at: got %v, want %v (post-gap event)", newStart, laterTime)
	}
}

// TestActiveWindowReanchorsToSnapshotBoundary is a regression for the case
// where a window was born under the calendar fallback (because no snapshot
// had supplied an authoritative boundary yet), then the userscript later
// started reporting the real reset time. ensureWeeklyWindow used to consult
// snapshots only at creation, leaving the active window stuck on the wrong
// boundary until it expired — which for weekly is up to 7 days.
func TestActiveWindowReanchorsToSnapshotBoundary(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	// Sunday April 26 2026 21:00 UTC, with no snapshots yet — the engine
	// will fall back to the calendar default for the weekly window.
	now := time.Date(2026, 4, 26, 21, 0, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("first UpdateWindows: %v", err)
	}

	var oldStart, oldEnds time.Time
	if err := s.DB().QueryRow(
		`SELECT started_at, ends_at FROM windows WHERE kind = 'weekly' AND closed = 0`,
	).Scan(&oldStart, &oldEnds); err != nil {
		t.Fatalf("query initial weekly window: %v", err)
	}
	wantOldEnds := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC) // calendar default
	if !oldEnds.Equal(wantOldEnds) {
		t.Fatalf("calendar-default ends_at: got %v, want %v", oldEnds, wantOldEnds)
	}

	// Userscript v0.3 starts reporting the real reset time (Friday 04:00 UTC,
	// = Thursday 11pm Eastern). The currently-active window should be
	// re-anchored, not left stuck on the calendar fallback.
	authoritativeEnds := time.Date(2026, 5, 1, 4, 0, 0, 0, time.UTC)
	weekly := 24.0
	if _, err := s.InsertQuotaSnapshot(
		now, now, "userscript",
		nil, nil,
		&weekly, &authoritativeEnds,
		nil,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("second UpdateWindows: %v", err)
	}

	var newStart, newEnds time.Time
	if err := s.DB().QueryRow(
		`SELECT started_at, ends_at FROM windows WHERE kind = 'weekly' AND closed = 0`,
	).Scan(&newStart, &newEnds); err != nil {
		t.Fatalf("query re-anchored weekly window: %v", err)
	}
	if !newEnds.Equal(authoritativeEnds) {
		t.Errorf("re-anchored ends_at: got %v, want %v", newEnds, authoritativeEnds)
	}
	wantNewStart := authoritativeEnds.Add(-7 * 24 * time.Hour)
	if !newStart.Equal(wantNewStart) {
		t.Errorf("re-anchored started_at: got %v, want %v", newStart, wantNewStart)
	}

	// Should not have created a second window; we updated in place.
	var count int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'weekly'`,
	).Scan(&count); err != nil {
		t.Fatalf("count windows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 weekly window after re-anchor, got %d", count)
	}
}

// TestExpiredSessionInactiveSnapshotSuppressesPhantomWindow confirms that when
// the most recent snapshot reports session_active=false after the prior session
// window has expired, no replacement (phantom) window is minted. Zero open
// session rows is the permitted state.
func TestExpiredSessionInactiveSnapshotSuppressesPhantomWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Seed the initial session window via an event.
	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Advance past the session window's end and ingest a snapshot reporting
	// the session as inactive.
	laterTime := now.Add(5*time.Hour + 1*time.Minute)
	engine.SetNow(func() time.Time { return laterTime })

	inactive := false
	if _, err := s.InsertQuotaSnapshot(
		laterTime, laterTime, "userscript",
		nil, nil,
		nil, nil,
		&inactive,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var openCount int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&openCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if openCount != 0 {
		t.Errorf("expected 0 open session windows when snapshot is inactive, got %d", openCount)
	}

	// The previously-active window should still be closed.
	var closedCount int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 1`,
	).Scan(&closedCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if closedCount != 1 {
		t.Errorf("expected 1 closed session window, got %d", closedCount)
	}
}

// TestExpiredSessionNullSessionActiveNoFreshEventLeavesWindowClosed confirms
// that under the event-evidence rule, a NULL session_active snapshot after an
// expired window does NOT mint a phantom replacement when there is no
// usage_event after the window's ends_at. The phantom-creation path that
// older code took on NULL has been replaced by event-evidence-only opening.
func TestExpiredSessionNullSessionActiveNoFreshEventLeavesWindowClosed(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	laterTime := now.Add(5*time.Hour + 1*time.Minute)
	engine.SetNow(func() time.Time { return laterTime })

	// Snapshot with session_active=nil → stored as NULL.
	if _, err := s.InsertQuotaSnapshot(
		laterTime, laterTime, "test",
		nil, nil,
		nil, nil,
		nil,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var openCount int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&openCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if openCount != 0 {
		t.Errorf("expected 0 open session windows (no fresh event after expiry), got %d", openCount)
	}

	var closedCount int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 1`,
	).Scan(&closedCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if closedCount != 1 {
		t.Errorf("expected 1 closed session window, got %d", closedCount)
	}
}

// TestExpiredSessionActiveSnapshotOpensNewWindow confirms that an active
// snapshot with a future session_window_ends after a prior expired window
// opens a new window anchored on that boundary.
func TestExpiredSessionActiveSnapshotOpensNewWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	laterTime := now.Add(5*time.Hour + 1*time.Minute)
	engine.SetNow(func() time.Time { return laterTime })

	active := true
	sessionEnds := laterTime.Add(4 * time.Hour)
	if _, err := s.InsertQuotaSnapshot(
		laterTime, laterTime, "userscript",
		nil, &sessionEnds,
		nil, nil,
		&active,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var startedAt, endsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT started_at, ends_at FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&startedAt, &endsAt); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if !endsAt.Equal(sessionEnds) {
		t.Errorf("expected new session window ends_at=%v (from snapshot), got %v", sessionEnds, endsAt)
	}
	wantStart := sessionEnds.Add(-5 * time.Hour)
	if !startedAt.Equal(wantStart) {
		t.Errorf("expected new session window started_at=%v, got %v", wantStart, startedAt)
	}
}

// TestActiveSessionInactiveSnapshotZeroUsedClosesWindowEarly is a regression
// test for the case where Anthropic's UI declares the session inactive AND
// reports zero usage in the current window — the window is effectively over
// before its calendar boundary, and the engine must close it early so
// event-anchored opening can later distinguish post-closure events.
func TestActiveSessionInactiveSnapshotZeroUsedClosesWindowEarly(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var windowID int64
	if err := s.DB().QueryRow(
		`SELECT id FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&windowID); err != nil {
		t.Fatalf("query initial window: %v", err)
	}

	// Still inside the original 5-hour window. Snapshot reports inactive
	// with 0% used: early-close.
	snapshotTime := now.Add(2 * time.Hour)
	engine.SetNow(func() time.Time { return snapshotTime })

	inactive := false
	usedZero := 0.0
	if _, err := s.InsertQuotaSnapshot(
		snapshotTime, snapshotTime, "userscript",
		&usedZero, nil,
		nil, nil,
		&inactive,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var closed int
	var endsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT closed, ends_at FROM windows WHERE id = ?`, windowID,
	).Scan(&closed, &endsAt); err != nil {
		t.Fatalf("query updated window: %v", err)
	}
	if closed != 1 {
		t.Errorf("expected window closed=1, got %d", closed)
	}
	if !endsAt.Equal(snapshotTime) {
		t.Errorf("expected ends_at=%v (snapshot observed_at), got %v", snapshotTime, endsAt)
	}
}

// TestActiveSessionInactiveSnapshotNonzeroUsedKeepsWindowOpen is the defensive
// contradiction case: Anthropic briefly flickers session_active=false while a
// session is opening. If used > 0, the window is not really closed; leave it.
func TestActiveSessionInactiveSnapshotNonzeroUsedKeepsWindowOpen(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var windowID int64
	var origEndsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT id, ends_at FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&windowID, &origEndsAt); err != nil {
		t.Fatalf("query initial window: %v", err)
	}

	snapshotTime := now.Add(2 * time.Hour)
	engine.SetNow(func() time.Time { return snapshotTime })

	inactive := false
	usedNonzero := 2.0
	if _, err := s.InsertQuotaSnapshot(
		snapshotTime, snapshotTime, "userscript",
		&usedNonzero, nil,
		nil, nil,
		&inactive,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var closed int
	var endsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT closed, ends_at FROM windows WHERE id = ?`, windowID,
	).Scan(&closed, &endsAt); err != nil {
		t.Fatalf("query updated window: %v", err)
	}
	if closed != 0 {
		t.Errorf("expected window to remain open (closed=0), got closed=%d", closed)
	}
	if !endsAt.Equal(origEndsAt) {
		t.Errorf("expected original ends_at=%v preserved, got %v", origEndsAt, endsAt)
	}
}

// TestActiveSessionMostRecentRulePrefersNewerActiveSnapshot confirms the
// most-recent-snapshot rule: an older inactive snapshot followed by a newer
// active snapshot must NOT close the window. The early-close decision is tied
// to the most recent snapshot, not any arbitrary inactive one.
func TestActiveSessionMostRecentRulePrefersNewerActiveSnapshot(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var windowID int64
	var origEndsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT id, ends_at FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&windowID, &origEndsAt); err != nil {
		t.Fatalf("query initial window: %v", err)
	}

	// Older inactive snapshot with 0% used — would trigger early-close on
	// its own, but is superseded by the newer active snapshot below.
	olderTime := now.Add(1 * time.Hour)
	inactive := false
	usedZero := 0.0
	if _, err := s.InsertQuotaSnapshot(
		olderTime, olderTime, "userscript",
		&usedZero, nil,
		nil, nil,
		&inactive,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert older snapshot: %v", err)
	}

	// Newer active snapshot — the most-recent rule means this is what the
	// engine consults.
	newerTime := now.Add(2 * time.Hour)
	engine.SetNow(func() time.Time { return newerTime })
	active := true
	usedNewer := 5.0
	if _, err := s.InsertQuotaSnapshot(
		newerTime, newerTime, "userscript",
		&usedNewer, nil,
		nil, nil,
		&active,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert newer snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var closed int
	var endsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT closed, ends_at FROM windows WHERE id = ?`, windowID,
	).Scan(&closed, &endsAt); err != nil {
		t.Fatalf("query updated window: %v", err)
	}
	if closed != 0 {
		t.Errorf("expected window to remain open (closed=0) under most-recent rule, got closed=%d", closed)
	}
	if !endsAt.Equal(origEndsAt) {
		t.Errorf("expected original ends_at=%v preserved, got %v", origEndsAt, endsAt)
	}
}

func TestBaselineFromSnapshot(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Two prior snapshots; the engine should pick the most recent one
	// at-or-before the window start. Values are session_used percentages.
	older := 12.0
	if _, err := s.InsertQuotaSnapshot(
		now.Add(-2*time.Hour), now.Add(-2*time.Hour), "test",
		&older, nil,
		nil, nil,
		nil,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("failed to insert older snapshot: %v", err)
	}

	newer := 28.0
	if _, err := s.InsertQuotaSnapshot(
		now.Add(-1*time.Minute), now.Add(-1*time.Minute), "test",
		&newer, nil,
		nil, nil,
		nil,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("failed to insert newer snapshot: %v", err)
	}

	// Insert an event that starts the window
	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	// Update windows
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Baseline should reflect the most recent prior snapshot.
	var baseline sql.NullFloat64
	var source string
	row := s.DB().QueryRow(`SELECT baseline_percent_used, baseline_source FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&baseline, &source); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if !baseline.Valid || baseline.Float64 != newer {
		t.Errorf("expected baseline %f (most recent snapshot), got %v", newer, baseline)
	}
	if source != "snapshot" {
		t.Errorf("expected baseline_source 'snapshot', got %q", source)
	}
}

func TestBaselineCorrection(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert an event to start the window with no preceding snapshot.
	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Capture the original window id so we can confirm the same row was
	// updated (not replaced).
	var windowID int64
	var initialBaseline sql.NullFloat64
	var initialSource string
	row := s.DB().QueryRow(`SELECT id, baseline_percent_used, baseline_source FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&windowID, &initialBaseline, &initialSource); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if initialBaseline.Valid {
		t.Errorf("expected nil baseline initially, got %v", initialBaseline.Float64)
	}
	if initialSource != "no_snapshot" {
		t.Errorf("expected initial source 'no_snapshot', got %q", initialSource)
	}

	// Snapshot arrives within the window with a new baseline.
	laterTime := now.Add(1 * time.Minute)
	engine.SetNow(func() time.Time { return laterTime })

	newBaseline := 18.0
	snapshotID, err := s.InsertQuotaSnapshot(
		laterTime, laterTime, "test",
		&newBaseline, nil,
		nil, nil,
		nil,
		nil,
		nil,
		"{}",
	)
	if err != nil {
		t.Fatalf("failed to insert snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var correctedID int64
	var baseline sql.NullFloat64
	var baselineSource string
	row = s.DB().QueryRow(`SELECT id, baseline_percent_used, baseline_source FROM windows WHERE kind = 'session' AND closed = 0`)
	if err := row.Scan(&correctedID, &baseline, &baselineSource); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if correctedID != windowID {
		t.Errorf("expected baseline correction to update existing window %d, got new id %d", windowID, correctedID)
	}
	if !baseline.Valid || baseline.Float64 != newBaseline {
		t.Errorf("expected baseline %f, got %v", newBaseline, baseline)
	}
	wantSource := fmt.Sprintf("snapshot:%d", snapshotID)
	if baselineSource != wantSource {
		t.Errorf("expected baseline_source %q, got %q", wantSource, baselineSource)
	}
}

// TestEventAfterNaturalExpiryOpensEventAnchoredWindow verifies the
// event-evidence rule for a naturally-expired session window: a usage_event
// arriving after the prior window's ends_at, with no recent inactive
// snapshot, opens a new window anchored at the event's timestamp.
func TestEventAfterNaturalExpiryOpensEventAnchoredWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert seeding event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("first UpdateWindows: %v", err)
	}

	// Advance well past the original window's ends_at = now+5h.
	postExpiry := now.Add(5*time.Hour + 30*time.Minute)
	engine.SetNow(func() time.Time { return postExpiry })

	// Fresh event arrives after expiry. No snapshot exists, so neither
	// findSessionBoundary nor phantom-suppression has anything to say.
	eventTime := postExpiry
	if _, err := s.InsertUsageEvent(
		eventTime, "test", "session-2", "msg-2", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert post-expiry event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("second UpdateWindows: %v", err)
	}

	var startedAt, endsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT started_at, ends_at FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&startedAt, &endsAt); err != nil {
		t.Fatalf("query new window: %v", err)
	}
	if !startedAt.Equal(eventTime) {
		t.Errorf("expected started_at=%v (event time), got %v", eventTime, startedAt)
	}
	wantEnds := eventTime.Add(5 * time.Hour)
	if !endsAt.Equal(wantEnds) {
		t.Errorf("expected ends_at=%v (event+5h), got %v", wantEnds, endsAt)
	}
}

// TestEventAfterEarlyClosedWindowOpensEventAnchoredWindow verifies the
// event-evidence rule for an early-closed window (closed=1, ends_at = the
// snapshot's observed_at, in the past). A usage_event newer than that
// ends_at — and no inactive snapshot more recent than the event — opens a
// new window anchored at the event.
func TestEventAfterEarlyClosedWindowOpensEventAnchoredWindow(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Open initial session window via an event.
	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert seeding event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("first UpdateWindows: %v", err)
	}

	// Inactive+zero-used snapshot arrives well within the original window;
	// the engine early-closes the window at this snapshot's observed_at.
	closeTime := now.Add(2 * time.Hour)
	engine.SetNow(func() time.Time { return closeTime })
	inactive := false
	usedZero := 0.0
	if _, err := s.InsertQuotaSnapshot(
		closeTime, closeTime, "userscript",
		&usedZero, nil,
		nil, nil,
		&inactive,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert inactive snapshot: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("early-close UpdateWindows: %v", err)
	}

	// Confirm the early-close happened so the test is exercising the
	// post-closure-event path, not natural expiry.
	var closedCount int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 1`,
	).Scan(&closedCount); err != nil {
		t.Fatalf("query closed count: %v", err)
	}
	if closedCount != 1 {
		t.Fatalf("expected 1 closed session window after early-close, got %d", closedCount)
	}

	// Resume: an active snapshot (no session_window_ends) supersedes the
	// inactive one — required to clear phantom suppression — and a fresh
	// event arrives. With no future session boundary in any snapshot, the
	// event-evidence path runs and must open at the event timestamp.
	eventTime := closeTime.Add(15 * time.Minute)
	engine.SetNow(func() time.Time { return eventTime })
	active := true
	if _, err := s.InsertQuotaSnapshot(
		eventTime, eventTime, "userscript",
		nil, nil,
		nil, nil,
		&active,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert active snapshot: %v", err)
	}
	if _, err := s.InsertUsageEvent(
		eventTime, "test", "session-2", "msg-2", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert post-close event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("post-close UpdateWindows: %v", err)
	}

	var startedAt, endsAt time.Time
	if err := s.DB().QueryRow(
		`SELECT started_at, ends_at FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&startedAt, &endsAt); err != nil {
		t.Fatalf("query new window: %v", err)
	}
	if !startedAt.Equal(eventTime) {
		t.Errorf("expected started_at=%v (event time), got %v", eventTime, startedAt)
	}
	wantEnds := eventTime.Add(5 * time.Hour)
	if !endsAt.Equal(wantEnds) {
		t.Errorf("expected ends_at=%v (event+5h), got %v", wantEnds, endsAt)
	}
}

// TestEventAnchoredWindowReanchorsOnSnapshotBoundary verifies that an
// event-anchored window subsequently re-anchors via reanchorIfStale once a
// snapshot supplies a real future session_window_ends.
func TestEventAnchoredWindowReanchorsOnSnapshotBoundary(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Event creates an event-anchored window: [now, now+5h).
	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("first UpdateWindows: %v", err)
	}

	var windowID int64
	if err := s.DB().QueryRow(
		`SELECT id FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&windowID); err != nil {
		t.Fatalf("query event-anchored window: %v", err)
	}

	// Snapshot with a real authoritative session_window_ends differing by
	// well more than the 2-minute tolerance.
	snapTime := now.Add(30 * time.Minute)
	engine.SetNow(func() time.Time { return snapTime })
	authoritativeEnds := now.Add(4*time.Hour + 12*time.Minute)
	active := true
	if _, err := s.InsertQuotaSnapshot(
		snapTime, snapTime, "userscript",
		nil, &authoritativeEnds,
		nil, nil,
		&active,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("re-anchor UpdateWindows: %v", err)
	}

	var newStart, newEnds time.Time
	var newID int64
	if err := s.DB().QueryRow(
		`SELECT id, started_at, ends_at FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&newID, &newStart, &newEnds); err != nil {
		t.Fatalf("query re-anchored window: %v", err)
	}
	if newID != windowID {
		t.Errorf("expected re-anchor in place (id=%d), got new id=%d", windowID, newID)
	}
	if !newEnds.Equal(authoritativeEnds) {
		t.Errorf("re-anchored ends_at: got %v, want %v", newEnds, authoritativeEnds)
	}
	wantStart := authoritativeEnds.Add(-5 * time.Hour)
	if !newStart.Equal(wantStart) {
		t.Errorf("re-anchored started_at: got %v, want %v", newStart, wantStart)
	}
}

// TestEventAnchoredWindowSeedsBaselineFromPriorSnapshot verifies that
// findBaseline picks up the latest pre-window snapshot to seed the
// event-anchored window's baseline_percent_used.
func TestEventAnchoredWindowSeedsBaselineFromPriorSnapshot(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Seed-then-expire an initial window to exercise the post-expiry
	// event-evidence path (rather than the no-prior-window path covered by
	// TestBaselineFromSnapshot).
	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert seeding event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("first UpdateWindows: %v", err)
	}

	postExpiry := now.Add(5*time.Hour + 30*time.Minute)

	// Two pre-window snapshots — engine should pick the more recent one.
	older := 8.0
	if _, err := s.InsertQuotaSnapshot(
		postExpiry.Add(-30*time.Minute), postExpiry.Add(-30*time.Minute), "userscript",
		&older, nil,
		nil, nil,
		nil,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert older snapshot: %v", err)
	}
	newer := 22.5
	if _, err := s.InsertQuotaSnapshot(
		postExpiry.Add(-1*time.Minute), postExpiry.Add(-1*time.Minute), "userscript",
		&newer, nil,
		nil, nil,
		nil,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert newer snapshot: %v", err)
	}

	// Fresh event after expiry → opens an event-anchored window.
	engine.SetNow(func() time.Time { return postExpiry })
	if _, err := s.InsertUsageEvent(
		postExpiry, "test", "session-2", "msg-2", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert post-expiry event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("second UpdateWindows: %v", err)
	}

	var baseline sql.NullFloat64
	var source string
	if err := s.DB().QueryRow(
		`SELECT baseline_percent_used, baseline_source FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&baseline, &source); err != nil {
		t.Fatalf("query baseline: %v", err)
	}
	if !baseline.Valid || baseline.Float64 != newer {
		t.Errorf("expected baseline=%v (latest pre-window snapshot), got %v", newer, baseline)
	}
	if source != "snapshot" {
		t.Errorf("expected baseline_source=%q, got %q", "snapshot", source)
	}
}

// TestEventEvidenceSuppressedByInactiveSnapshot verifies that phantom
// suppression remains authoritative: when the most recent snapshot has
// session_active=false, the engine must not open a window even if a
// usage_event would otherwise satisfy the event-evidence rule.
func TestEventEvidenceSuppressedByInactiveSnapshot(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Seed-then-expire an initial window.
	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert seeding event: %v", err)
	}
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("first UpdateWindows: %v", err)
	}

	postExpiry := now.Add(5*time.Hour + 30*time.Minute)
	engine.SetNow(func() time.Time { return postExpiry })

	// Fresh event after expiry — would normally open a new window …
	if _, err := s.InsertUsageEvent(
		postExpiry, "test", "session-2", "msg-2", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	); err != nil {
		t.Fatalf("insert post-expiry event: %v", err)
	}

	// … but the most recent snapshot reports session_active=false, so
	// phantom suppression must win.
	inactive := false
	if _, err := s.InsertQuotaSnapshot(
		postExpiry, postExpiry, "userscript",
		nil, nil,
		nil, nil,
		&inactive,
		nil,
		nil,
		"{}",
	); err != nil {
		t.Fatalf("insert inactive snapshot: %v", err)
	}

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows: %v", err)
	}

	var openCount int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&openCount); err != nil {
		t.Fatalf("query open count: %v", err)
	}
	if openCount != 0 {
		t.Errorf("expected 0 open session windows (phantom suppression wins over event evidence), got %d", openCount)
	}
}

// Plateau compaction in the store slides the latest row's observed_at and
// received_at forward in place. The window engine's snapshot lookups order
// by observed_at, so they should still see the latest plateau values
// (boundary, session_active, session_used) after a slide — and the slid
// observed_at should reflect the latest sighting.
func TestSnapshotLookupsHonorSlidObservedAt(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	sessionEnds := t0.Add(5 * time.Hour)
	active := true

	if _, err := s.InsertQuotaSnapshot(
		t0, t0, "userscript",
		floatPtr(25.0), &sessionEnds,
		nil, nil,
		&active,
		nil,
		boolPtr(false),
		"{}",
	); err != nil {
		t.Fatalf("insert start: %v", err)
	}

	tLater := t0.Add(15 * time.Minute)
	if _, err := s.InsertQuotaSnapshot(
		tLater, tLater, "userscript",
		floatPtr(25.0), &sessionEnds,
		nil, nil,
		&active,
		nil,
		boolPtr(true),
		"{}",
	); err != nil {
		t.Fatalf("insert continuation: %v", err)
	}

	// Drive a final plateau slide.
	tLatest := t0.Add(45 * time.Minute)
	if _, err := s.InsertQuotaSnapshot(
		tLatest, tLatest, "userscript",
		floatPtr(25.0), &sessionEnds,
		nil, nil,
		&active,
		nil,
		boolPtr(true),
		"{}",
	); err != nil {
		t.Fatalf("insert second continuation: %v", err)
	}

	// Sanity: there should be exactly two rows (start + slid continuation).
	var rowCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 2 {
		t.Fatalf("expected 2 rows after plateau, got %d", rowCount)
	}

	gotBoundary, err := engine.findSessionBoundary()
	if err != nil {
		t.Fatalf("findSessionBoundary: %v", err)
	}
	if !gotBoundary.Equal(sessionEnds) {
		t.Errorf("findSessionBoundary: got %v, want %v", gotBoundary, sessionEnds)
	}

	gotActive, gotUsed, gotObserved, err := engine.findMostRecentSessionActive()
	if err != nil {
		t.Fatalf("findMostRecentSessionActive: %v", err)
	}
	if gotActive == nil || !*gotActive {
		t.Errorf("session_active: got %v, want true", gotActive)
	}
	if gotUsed == nil || *gotUsed != 25.0 {
		t.Errorf("session_used: got %v, want 25.0", gotUsed)
	}
	if !gotObserved.Equal(tLatest) {
		t.Errorf("observed_at after slide: got %v, want %v", gotObserved, tLatest)
	}
}

func floatPtr(f float64) *float64 {
	return &f
}

func boolPtr(b bool) *bool {
	return &b
}
