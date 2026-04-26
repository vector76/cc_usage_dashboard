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

func TestFirstEventCreates5HourWindow(t *testing.T) {
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

	// Check that a 5-hour window was created
	var count int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 five_hour window, got %d", count)
	}

	// Check window times
	var startedAt, endsAt time.Time
	row = s.DB().QueryRow(`SELECT started_at, ends_at FROM windows WHERE kind = 'five_hour' AND closed = 0`)
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
		nil, nil, nil,
		nil, nil, &weeklyEnds,
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

	// Wednesday April 22 2026 12:00 UTC. Default Sunday boundary is the
	// midnight of the upcoming Monday minus 24h: Sunday April 26 00:00 UTC.
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

	wantEnds := time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC)
	if !endsAt.Equal(wantEnds) {
		t.Errorf("default weekly ends_at: got %v, want %v", endsAt, wantEnds)
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

	// Should still have only 1 five-hour window
	var count int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected 1 five_hour window, got %d", count)
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
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'five_hour' AND closed = 1`)
	if err := row.Scan(&closedCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if closedCount != 1 {
		t.Errorf("expected 1 closed window, got %d", closedCount)
	}

	// Check that a new window exists.
	var openCount int
	row = s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&openCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if openCount != 1 {
		t.Errorf("expected 1 open window, got %d", openCount)
	}

	// And that it starts at the post-gap event (not just at `now`) — i.e.
	// findFirstEventAfterGap picked up the event.
	var newStart time.Time
	row = s.DB().QueryRow(`SELECT started_at FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&newStart); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if !newStart.Equal(laterTime) {
		t.Errorf("new window started_at: got %v, want %v (post-gap event)", newStart, laterTime)
	}
}

func TestBaselineFromSnapshot(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Two prior snapshots; the engine should pick the most recent one
	// at-or-before the window start.
	older := 100.0
	if _, err := s.InsertQuotaSnapshot(
		now.Add(-2*time.Hour), now.Add(-2*time.Hour), "test",
		nil, &older, nil,
		nil, nil, nil,
		"{}",
	); err != nil {
		t.Fatalf("failed to insert older snapshot: %v", err)
	}

	newer := 250.0
	if _, err := s.InsertQuotaSnapshot(
		now.Add(-1*time.Minute), now.Add(-1*time.Minute), "test",
		nil, &newer, nil,
		nil, nil, nil,
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
	row := s.DB().QueryRow(`SELECT baseline_total, baseline_source FROM windows WHERE kind = 'five_hour' AND closed = 0`)
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
	row := s.DB().QueryRow(`SELECT id, baseline_total, baseline_source FROM windows WHERE kind = 'five_hour' AND closed = 0`)
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

	newBaseline := 200.0
	snapshotID, err := s.InsertQuotaSnapshot(
		laterTime, laterTime, "test",
		nil, &newBaseline, nil,
		nil, nil, nil,
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
	row = s.DB().QueryRow(`SELECT id, baseline_total, baseline_source FROM windows WHERE kind = 'five_hour' AND closed = 0`)
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

func TestDriftCalculation(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert a baseline snapshot just before the window
	baseline := 1000.0
	if _, err := s.InsertQuotaSnapshot(
		now.Add(-1*time.Minute), now.Add(-1*time.Minute), "test",
		nil, &baseline, nil,
		nil, nil, nil,
		"{}",
	); err != nil {
		t.Fatalf("failed to insert snapshot: %v", err)
	}

	// Trigger window creation now so the window starts at `now` and
	// ends at `now + 5h`.
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Two events within the window plus events outside (before and after)
	// that must be excluded from the drift calculation.
	costIn1 := 100.0
	costIn2 := 50.0
	costBefore := 999.0
	costAfter := 999.0

	if _, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-in-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, &costIn1, "reported", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	if _, err := s.InsertUsageEvent(
		now.Add(1*time.Minute), "test", "session-1", "msg-in-2", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, &costIn2, "reported", "{}",
	); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	if _, err := s.InsertUsageEvent(
		now.Add(-30*time.Minute), "test", "session-1", "msg-before", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, &costBefore, "reported", "{}",
	); err != nil {
		t.Fatalf("failed to insert pre-window event: %v", err)
	}

	if _, err := s.InsertUsageEvent(
		now.Add(6*time.Hour), "test", "session-1", "msg-after", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, &costAfter, "reported", "{}",
	); err != nil {
		t.Fatalf("failed to insert post-window event: %v", err)
	}

	// Get the active window
	var windowID int64
	row := s.DB().QueryRow(`SELECT id FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&windowID); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	drift, err := engine.Drift(windowID)
	if err != nil {
		t.Fatalf("Drift failed: %v", err)
	}

	// Expected: baseline (1000) - consumed in-window (150) = 850.
	// Out-of-window events (999 + 999) must not contribute.
	expectedDrift := 850.0
	if drift == nil {
		t.Fatalf("expected non-nil drift")
	}
	if *drift != expectedDrift {
		t.Errorf("expected drift %f, got %f (out-of-window events leaked?)", expectedDrift, *drift)
	}
}

func TestDriftNilBaseline(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	var windowID int64
	row := s.DB().QueryRow(`SELECT id FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&windowID); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	drift, err := engine.Drift(windowID)
	if err != nil {
		t.Fatalf("Drift failed: %v", err)
	}
	if drift != nil {
		t.Errorf("expected nil drift when baseline is nil, got %f", *drift)
	}
}
