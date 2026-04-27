package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSnapshotRejectsOutOfRangeTimestamps verifies that *_window_ends
// values pointing at the year 9999 (or far-past dates) are rejected with
// 400 instead of being persisted. Without this guard a hostile or buggy
// snapshot could pin a window's reset boundary far in the future and
// freeze the slack signal.
func TestSnapshotRejectsOutOfRangeTimestamps(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	farPast := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		mutate  func(*SnapshotRequest)
		wantErr string
	}{
		{
			name: "session_window_ends far future",
			mutate: func(r *SnapshotRequest) {
				r.SessionWindowEnds = &farFuture
			},
			wantErr: "session_window_ends too far in the future",
		},
		{
			name: "weekly_window_ends far future",
			mutate: func(r *SnapshotRequest) {
				r.WeeklyWindowEnds = &farFuture
			},
			wantErr: "weekly_window_ends too far in the future",
		},
		{
			name: "session_window_ends far past",
			mutate: func(r *SnapshotRequest) {
				r.SessionWindowEnds = &farPast
			},
			wantErr: "session_window_ends too far in the past",
		},
		{
			name: "observed_at far future",
			mutate: func(r *SnapshotRequest) {
				future := now.Add(10 * time.Hour)
				r.ObservedAt = future
			},
			wantErr: "observed_at too far in the future",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &SnapshotRequest{ObservedAt: now}
			tc.mutate(req)
			err := validateSnapshotTimestamps(req, now)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestSnapshotAcceptsInBoundsTimestamps verifies the validator passes the
// values the userscript actually produces (now-ish observation, ~5h
// session reset, ~7d weekly reset).
func TestSnapshotAcceptsInBoundsTimestamps(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	sessEnd := now.Add(5 * time.Hour)
	weekEnd := now.Add(6 * 24 * time.Hour)
	req := &SnapshotRequest{
		ObservedAt:        now,
		SessionWindowEnds: &sessEnd,
		WeeklyWindowEnds:  &weekEnd,
	}
	if err := validateSnapshotTimestamps(req, now); err != nil {
		t.Errorf("validateSnapshotTimestamps rejected a normal snapshot: %v", err)
	}

	// Zero-valued timestamps must be tolerated (userscript can omit a
	// reset hint when the DOM doesn't expose one).
	emptyReq := &SnapshotRequest{}
	if err := validateSnapshotTimestamps(emptyReq, now); err != nil {
		t.Errorf("validateSnapshotTimestamps rejected an all-zero snapshot: %v", err)
	}
}

func TestSnapshotCreatesWindow(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	fixed := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	srv.windowsEngine.SetNow(func() time.Time { return fixed })
	// The snapshot handler now bounds-checks ObservedAt against
	// Server.now; align that clock with the fixture so the test
	// payload's ObservedAt isn't flagged as "too far in the past".
	srv.SetNow(func() time.Time { return fixed })

	used := 20.0
	payload := SnapshotRequest{
		ObservedAt:  fixed,
		Source:      "userscript",
		SessionUsed: &used,
	}

	body, _ := json.Marshal(payload)
	req := jsonPOST("/snapshot", body)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%s)", w.Code, w.Body.String())
	}

	var count int
	if err := testStore.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session'`).Scan(&count); err != nil {
		t.Fatalf("failed to count windows: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one session window after snapshot, got 0")
	}
}

func TestSnapshotInWindowUpdatesBaseline(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	fixed := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	srv.windowsEngine.SetNow(func() time.Time { return fixed })
	// The snapshot handler now bounds-checks ObservedAt against
	// Server.now; align that clock with the fixture so the test
	// payload's ObservedAt isn't flagged as "too far in the past".
	srv.SetNow(func() time.Time { return fixed })

	// First snapshot: establishes the active window. Observed at the same
	// instant as the engine's now() so that it falls within [startedAt, endsAt).
	first := 5.0
	firstPayload := SnapshotRequest{
		ObservedAt:  fixed,
		Source:      "userscript",
		SessionUsed: &first,
	}
	body, _ := json.Marshal(firstPayload)
	req := jsonPOST("/snapshot", body)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first snapshot failed: status=%d body=%s", w.Code, w.Body.String())
	}

	var windowID int64
	if err := testStore.DB().QueryRow(
		`SELECT id FROM windows WHERE kind = 'session' AND closed = 0`,
	).Scan(&windowID); err != nil {
		t.Fatalf("failed to read window: %v", err)
	}

	// Second snapshot: still inside the window, with higher % used.
	second := 12.0
	secondPayload := SnapshotRequest{
		ObservedAt:  fixed.Add(1 * time.Minute),
		Source:      "userscript",
		SessionUsed: &second,
	}
	body, _ = json.Marshal(secondPayload)
	req = jsonPOST("/snapshot", body)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second snapshot failed: status=%d body=%s", w.Code, w.Body.String())
	}

	var baseline float64
	var source string
	if err := testStore.DB().QueryRow(
		`SELECT baseline_percent_used, baseline_source FROM windows WHERE id = ?`, windowID,
	).Scan(&baseline, &source); err != nil {
		t.Fatalf("failed to read updated window: %v", err)
	}

	if baseline != second {
		t.Errorf("expected baseline_percent_used=%v after in-window snapshot, got %v", second, baseline)
	}
	if !strings.HasPrefix(source, "snapshot:") {
		t.Errorf("expected baseline_source to start with 'snapshot:', got %q", source)
	}

	var count int
	if err := testStore.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'session'`).Scan(&count); err != nil {
		t.Fatalf("failed to count windows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 session window, got %d", count)
	}
}
