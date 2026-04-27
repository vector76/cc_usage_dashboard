package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboardIndexHTML(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	for _, path := range []string{"/", "/dashboard"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			ct := w.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Errorf("expected text/html content type, got %q", ct)
			}
			if w.Body.Len() == 0 {
				t.Error("expected non-empty body")
			}
			body := w.Body.String()
			if !strings.Contains(body, "<html") {
				snippet := body
				if len(snippet) > 120 {
					snippet = snippet[:120]
				}
				t.Errorf("response body does not look like HTML: %q", snippet)
			}
		})
	}
}

func TestDashboardStateJSONShape(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// Seed a parse error and an event so the state has something to report.
	if _, err := testStore.InsertParseError(time.Now().UTC(), "tailer", "bad", "{}"); err != nil {
		t.Fatalf("insert parse error: %v", err)
	}
	cost := 0.10
	if _, err := testStore.InsertUsageEvent(
		time.Now().UTC(), "api", "session-d1", "msg-d1", "/proj", "model-x",
		1000, 500, 0, 0, &cost, "reported", "{}",
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/dashboard/state", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected application/json, got %q", ct)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody=%s", err, w.Body.String())
	}

	for _, key := range []string{
		"now",
		"session",
		"weekly",
		"last_snapshot_age_seconds",
		"parse_errors_24h",
		"paused",
		"slack_release_recommended",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing documented field %q", key)
		}
	}

	// Decode the typed fields we care about.
	var pe int64
	if err := json.Unmarshal(raw["parse_errors_24h"], &pe); err != nil {
		t.Fatalf("parse_errors_24h not an int: %v", err)
	}
	if pe < 1 {
		t.Errorf("expected parse_errors_24h >= 1, got %d", pe)
	}

	var paused bool
	if err := json.Unmarshal(raw["paused"], &paused); err != nil {
		t.Fatalf("paused not a bool: %v", err)
	}
	if paused {
		t.Error("expected paused=false on fresh server")
	}
}

