package windows

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/anthropics/usage-dashboard/internal/store"
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

	// Check that a new window exists
	var openCount int
	row = s.DB().QueryRow(`SELECT COUNT(*) FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&openCount); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if openCount != 1 {
		t.Errorf("expected 1 open window, got %d", openCount)
	}
}

func TestBaselineFromSnapshot(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert a snapshot BEFORE the window
	snapshotTime := now.Add(-1 * time.Minute)
	baselineVal := 100.0
	_, err := s.InsertQuotaSnapshot(
		snapshotTime, snapshotTime, "test",
		nil, &baselineVal, nil,
		nil, nil, nil,
		"{}",
	)
	if err != nil {
		t.Fatalf("failed to insert snapshot: %v", err)
	}

	// Insert an event that starts the window
	_, err = s.InsertUsageEvent(
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

	// Check that the baseline was set from the snapshot
	var baseline sql.NullFloat64
	row := s.DB().QueryRow(`SELECT baseline_total FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&baseline); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if !baseline.Valid || baseline.Float64 != baselineVal {
		t.Errorf("expected baseline %f, got %v", baselineVal, baseline)
	}
}

func TestBaselineCorrection(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert an event to start the window
	_, err := s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, nil, "", "{}",
	)
	if err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	// Update windows with initial baseline
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Now insert a snapshot WITHIN the window with a new baseline
	laterTime := now.Add(1 * time.Minute)
	engine.SetNow(func() time.Time { return laterTime })

	newBaseline := 200.0
	_, err = s.InsertQuotaSnapshot(
		laterTime, laterTime, "test",
		nil, &newBaseline, nil,
		nil, nil, nil,
		"{}",
	)
	if err != nil {
		t.Fatalf("failed to insert snapshot: %v", err)
	}

	// Update windows - should correct the baseline
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Check that the baseline was updated
	var baseline sql.NullFloat64
	var baselineSource string
	row := s.DB().QueryRow(`SELECT baseline_total, baseline_source FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&baseline, &baselineSource); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if !baseline.Valid || baseline.Float64 != newBaseline {
		t.Errorf("expected baseline %f, got %v", newBaseline, baseline)
	}

	if !isValidSnapshotSource(baselineSource) {
		t.Errorf("expected baseline_source to contain 'snapshot:', got %s", baselineSource)
	}
}

func isValidSnapshotSource(s string) bool {
	// Check if it starts with "snapshot:"
	return len(s) > 9 && s[:9] == "snapshot:"
}

func TestDriftCalculation(t *testing.T) {
	engine, s := createTestEngine(t)
	defer s.Close()

	now := time.Date(2026, 4, 26, 10, 30, 0, 0, time.UTC)
	engine.SetNow(func() time.Time { return now })

	// Insert a baseline snapshot
	baseline := 1000.0
	_, err := s.InsertQuotaSnapshot(
		now.Add(-1*time.Minute), now.Add(-1*time.Minute), "test",
		nil, &baseline, nil,
		nil, nil, nil,
		"{}",
	)
	if err != nil {
		t.Fatalf("failed to insert snapshot: %v", err)
	}

	// Insert events
	cost1 := 100.0
	cost2 := 50.0

	_, err = s.InsertUsageEvent(
		now, "test", "session-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, &cost1, "reported", "{}",
	)
	if err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	_, err = s.InsertUsageEvent(
		now.Add(1*time.Minute), "test", "session-1", "msg-2", "", "claude-3-5-sonnet-20241022",
		100, 50, 0, 0, &cost2, "reported", "{}",
	)
	if err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}

	// Update windows
	if err := engine.UpdateWindows(); err != nil {
		t.Fatalf("UpdateWindows failed: %v", err)
	}

	// Get the active window
	var windowID int64
	var startedAt, endsAt time.Time
	var baselineTotal sql.NullFloat64
	row := s.DB().QueryRow(`SELECT id, started_at, ends_at, baseline_total FROM windows WHERE kind = 'five_hour' AND closed = 0`)
	if err := row.Scan(&windowID, &startedAt, &endsAt, &baselineTotal); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	// Calculate drift
	drift, err := engine.Drift(windowID)
	if err != nil {
		t.Fatalf("Drift failed: %v", err)
	}

	// Expected: baseline (1000) - consumed (150) = 850
	expectedDrift := 850.0
	if drift == nil || *drift != expectedDrift {
		t.Errorf("expected drift %f, got %v", expectedDrift, drift)
	}
}
