package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A forwarded event carries the sending machine's clock. The windows engine
// anchors a new session window directly on MAX(usage_events.occurred_at)
// (see internal/windows/windows.go findEventEvidenceForOpen: startTime =
// eventTime, endsAt = eventTime+5h), so a future-dated event does not merely
// sort oddly — it mints a window in the future that stays the maximum until
// real time catches up. The bound is deliberately asymmetric: tight ahead,
// generous behind.
func TestHandleLogRejectsFutureOccurredAt(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	future := time.Now().Add(maxLogOccurredFuture + time.Hour)
	payload := LogPostRequest{
		OccurredAt:   &future,
		InputTokens:  1000,
		OutputTokens: 500,
		SessionID:    "session-future",
		MessageID:    "msg-future",
	}

	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, jsonPOST("/log", body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a future occurred_at, got %d", w.Code)
	}

	var count int
	if err := testStore.DB().QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 0 {
		t.Errorf("rejected event must not be stored, found %d rows", count)
	}
}

// A clock reset to the epoch (dead CMOS battery, a VM restored from an old
// image) is the past-side failure this guards. The bound has to sit far
// enough back to never catch a legitimate backfill.
func TestHandleLogRejectsAncientOccurredAt(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	ancient := time.Now().Add(-maxLogOccurredPast - 24*time.Hour)
	payload := LogPostRequest{
		OccurredAt:   &ancient,
		InputTokens:  1000,
		OutputTokens: 500,
		SessionID:    "session-ancient",
		MessageID:    "msg-ancient",
	}

	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, jsonPOST("/log", body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an ancient occurred_at, got %d", w.Code)
	}
}

// The Stop hook re-walks whole transcripts and legitimately re-posts events
// that are days old. Those must keep working — the filter exists to catch a
// broken clock, not to impose a freshness policy on backfills.
func TestHandleLogAcceptsOldBackfillOccurredAt(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	old := time.Now().Add(-30 * 24 * time.Hour)
	payload := LogPostRequest{
		OccurredAt:   &old,
		InputTokens:  1000,
		OutputTokens: 500,
		SessionID:    "session-backfill",
		MessageID:    "msg-backfill",
	}

	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, jsonPOST("/log", body))

	if w.Code != http.StatusOK {
		t.Fatalf("expected a month-old backfill to be accepted, got %d", w.Code)
	}
}

// Small drift is normal on a VM and is explicitly NOT corrected — the
// project accepts the error rather than silently rewriting timestamps.
func TestHandleLogAcceptsSmallSkew(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	skewed := time.Now().Add(2 * time.Minute)
	payload := LogPostRequest{
		OccurredAt:   &skewed,
		InputTokens:  1000,
		OutputTokens: 500,
		SessionID:    "session-skew",
		MessageID:    "msg-skew",
	}

	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, jsonPOST("/log", body))

	if w.Code != http.StatusOK {
		t.Fatalf("expected a slightly-ahead clock to be accepted, got %d", w.Code)
	}
}

// Omitting occurred_at entirely stays valid: the handler falls back to
// time.Now() for manual callers.
func TestHandleLogAcceptsAbsentOccurredAt(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	payload := LogPostRequest{
		InputTokens:  1000,
		OutputTokens: 500,
		SessionID:    "session-none",
		MessageID:    "msg-none",
	}

	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, jsonPOST("/log", body))

	if w.Code != http.StatusOK {
		t.Fatalf("expected an absent occurred_at to be accepted, got %d", w.Code)
	}
}
