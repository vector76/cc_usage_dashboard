package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func insertWindow(t *testing.T, db *sql.DB, kind string, startedAt, endsAt time.Time, baselineTotal float64) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO windows (kind, started_at, ends_at, baseline_total, baseline_source, closed)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		kind, store.FormatTime(startedAt), store.FormatTime(endsAt), baselineTotal, "snapshot:1",
	)
	if err != nil {
		t.Fatalf("insert window: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

// TestHandleSlackQueryShape verifies GET /slack returns the documented JSON
// shape: top-level keys and gate keys per docs/slack-indicator.md.
func TestHandleSlackQueryShape(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	req := httptest.NewRequest("GET", "/slack", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	wantTopKeys := []string{
		"now", "session", "weekly",
		"slack_combined_fraction", "priority_quiet_for_seconds",
		"paused", "release_recommended", "gates",
	}
	for _, k := range wantTopKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing top-level key %q in response: %s", k, w.Body.String())
		}
	}

	var gates map[string]bool
	if err := json.Unmarshal(raw["gates"], &gates); err != nil {
		t.Fatalf("decode gates: %v", err)
	}
	wantGateKeys := []string{"headroom", "priority_quiet", "baseline_freshness", "not_paused"}
	for _, k := range wantGateKeys {
		if _, ok := gates[k]; !ok {
			t.Errorf("missing gate %q in gates: %v", k, gates)
		}
	}
}

// TestHandleSlackQueryWindowFields verifies that when an active window exists,
// the window block uses the documented field names.
func TestHandleSlackQueryWindowFields(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Now().UTC()
	insertWindow(t, testStore.DB(), "session", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0)

	req := httptest.NewRequest("GET", "/slack", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var raw struct {
		Session map[string]json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw.Session == nil {
		t.Fatalf("expected session block, got null")
	}

	wantKeys := []string{"window_start", "window_end", "quota_total", "consumed", "expected", "slack", "slack_fraction"}
	for _, k := range wantKeys {
		if _, ok := raw.Session[k]; !ok {
			t.Errorf("missing session field %q: %s", k, w.Body.String())
		}
	}
}

// TestHandleSlackReleaseMissingFields verifies HTTP 400 for missing required
// fields.
func TestHandleSlackReleaseMissingFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing job_tag", `{"released_at":"2026-04-26T12:00:00Z"}`},
		{"missing released_at", `{"job_tag":"nightly-lint"}`},
		{"invalid JSON", `not-json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, testStore := createTestServer(t)
			defer testStore.Close()

			req := httptest.NewRequest("POST", "/slack/release", bytes.NewReader([]byte(tt.body)))
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleSlackReleaseNoActiveWindow verifies HTTP 409 with the documented
// error body when no window of the requested kind contains released_at.
func TestHandleSlackReleaseNoActiveWindow(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	body := `{"released_at":"2026-04-26T12:00:00Z","job_tag":"nightly-lint"}`
	req := httptest.NewRequest("POST", "/slack/release", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "no active window" {
		t.Errorf("expected error=\"no active window\", got %q", resp["error"])
	}
}

// TestHandleSlackReleaseWindowKindWeekly verifies window_kind="weekly" picks
// the weekly window (and a session-only DB returns 409 for weekly).
func TestHandleSlackReleaseWindowKindWeekly(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Now().UTC()
	weeklyID := insertWindow(t, testStore.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 5000.0)
	insertWindow(t, testStore.DB(), "session", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0)

	body, _ := json.Marshal(map[string]any{
		"released_at": now.Format(time.RFC3339Nano),
		"job_tag":     "weekly-job",
		"window_kind": "weekly",
	})
	req := httptest.NewRequest("POST", "/slack/release", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	var got struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var windowID int64
	if err := testStore.DB().QueryRow(
		`SELECT window_id FROM slack_releases WHERE id = ?`, got.ID,
	).Scan(&windowID); err != nil {
		t.Fatalf("query slack_releases: %v", err)
	}
	if windowID != weeklyID {
		t.Errorf("window_id: got %d, want weekly id %d", windowID, weeklyID)
	}
}

// TestHandleSlackReleaseWindowKindNoMatch verifies that when window_kind
// requests a kind for which no active window exists, we return 409 even if a
// window of the *other* kind exists.
func TestHandleSlackReleaseWindowKindNoMatch(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Now().UTC()
	// Only weekly exists; request session.
	insertWindow(t, testStore.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 5000.0)

	body, _ := json.Marshal(map[string]any{
		"released_at": now.Format(time.RFC3339Nano),
		"job_tag":     "session-job",
		"window_kind": "session",
	})
	req := httptest.NewRequest("POST", "/slack/release", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestSlackPausePersistsAcrossRequests verifies the calculator's pause flag
// is preserved between two GET /slack calls (i.e. the Server reuses one
// Calculator instance rather than constructing a fresh one per request).
func TestSlackPausePersistsAcrossRequests(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// Bypass the (currently absent) HTTP pause endpoint by toggling on the
	// shared calculator directly.
	srv.slackCalc.SetPaused(true)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/slack", nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d", i, w.Code)
		}
		var resp struct {
			Paused bool            `json:"paused"`
			Gates  map[string]bool `json:"gates"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("call %d: decode: %v", i, err)
		}
		if !resp.Paused {
			t.Errorf("call %d: expected paused=true (pause must persist), got false", i)
		}
		if resp.Gates["not_paused"] {
			t.Errorf("call %d: expected not_paused gate=false when paused", i)
		}
	}
}
