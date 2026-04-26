package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSnapshotCreatesWindow(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	fixed := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	srv.windowsEngine.SetNow(func() time.Time { return fixed })

	total := 100.0
	remaining := 80.0
	payload := SnapshotRequest{
		ObservedAt:        fixed,
		Source:            "userscript",
		FiveHourRemaining: &remaining,
		FiveHourTotal:     &total,
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/snapshot", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body=%s)", w.Code, w.Body.String())
	}

	var count int
	if err := testStore.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'five_hour'`).Scan(&count); err != nil {
		t.Fatalf("failed to count windows: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one five_hour window after snapshot, got 0")
	}
}

func TestSnapshotInWindowUpdatesBaseline(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	fixed := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	srv.windowsEngine.SetNow(func() time.Time { return fixed })

	// First snapshot: establishes the active window. Observed at the same
	// instant as the engine's now() so that it falls within [startedAt, endsAt).
	first := 100.0
	firstPayload := SnapshotRequest{
		ObservedAt:    fixed,
		Source:        "userscript",
		FiveHourTotal: &first,
	}
	body, _ := json.Marshal(firstPayload)
	req := httptest.NewRequest("POST", "/snapshot", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first snapshot failed: status=%d body=%s", w.Code, w.Body.String())
	}

	var windowID int64
	if err := testStore.DB().QueryRow(
		`SELECT id FROM windows WHERE kind = 'five_hour' AND closed = 0`,
	).Scan(&windowID); err != nil {
		t.Fatalf("failed to read window: %v", err)
	}

	// Second snapshot: still inside the window, with a smaller total.
	second := 75.0
	secondPayload := SnapshotRequest{
		ObservedAt:    fixed.Add(1 * time.Minute),
		Source:        "userscript",
		FiveHourTotal: &second,
	}
	body, _ = json.Marshal(secondPayload)
	req = httptest.NewRequest("POST", "/snapshot", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second snapshot failed: status=%d body=%s", w.Code, w.Body.String())
	}

	var baseline float64
	var source string
	if err := testStore.DB().QueryRow(
		`SELECT baseline_total, baseline_source FROM windows WHERE id = ?`, windowID,
	).Scan(&baseline, &source); err != nil {
		t.Fatalf("failed to read updated window: %v", err)
	}

	if baseline != second {
		t.Errorf("expected baseline_total=%v after in-window snapshot, got %v", second, baseline)
	}
	if !strings.HasPrefix(source, "snapshot:") {
		t.Errorf("expected baseline_source to start with 'snapshot:', got %q", source)
	}

	var count int
	if err := testStore.DB().QueryRow(
		`SELECT COUNT(*) FROM windows WHERE kind = 'five_hour'`).Scan(&count); err != nil {
		t.Fatalf("failed to count windows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 five_hour window, got %d", count)
	}
}
