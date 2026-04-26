package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func TestMetricsLogIncrementsEventsIngested(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	tests := []struct {
		name         string
		source       string
		expectSource string
	}{
		{"explicit_api_source", "api", "api"},
		{"explicit_tailer_source", "tailer", "tailer"},
		{"empty_source_defaults_to_api", "", "api"}, // handleLog defaults to "api"
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := srv.metrics.EventsIngested(tt.expectSource)

			payload := LogPostRequest{
				InputTokens:  1000,
				OutputTokens: 500,
				SessionID:    "session-" + tt.name,
				MessageID:    "msg-" + tt.name,
				Source:       tt.source,
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}

			after := srv.metrics.EventsIngested(tt.expectSource)
			if after != before+1 {
				t.Errorf("events_ingested_total{source=%q}: expected %d, got %d", tt.expectSource, before+1, after)
			}
		})
	}
}

func TestMetricsLogFailureDoesNotIncrement(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	before := srv.metrics.EventsIngested("api")

	// Missing required fields → 400
	payload := LogPostRequest{SessionID: "x"}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	after := srv.metrics.EventsIngested("api")
	if after != before {
		t.Errorf("counter should not increment on failure: before=%d after=%d", before, after)
	}
}

func TestMetricsParseErrorIncrements(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	before := srv.metrics.ParseErrors.Load()

	payload := ParseErrorRequest{
		Source:  "tailer",
		Reason:  "bad json",
		Payload: "{",
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/parse_error", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	after := srv.metrics.ParseErrors.Load()
	if after != before+1 {
		t.Errorf("parse_errors_total: expected %d, got %d", before+1, after)
	}
}

func TestMetricsSnapshotIncrements(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	before := srv.metrics.SnapshotsReceived.Load()

	now := time.Now().UTC()
	payload := SnapshotRequest{
		ObservedAt: now,
		Source:     "userscript",
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/snapshot", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	after := srv.metrics.SnapshotsReceived.Load()
	if after != before+1 {
		t.Errorf("snapshots_received_total: expected %d, got %d", before+1, after)
	}
}

func TestMetricsSlackQueryIncrements(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	before := srv.metrics.SlackQueries.Load()

	req := httptest.NewRequest("GET", "/slack", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	after := srv.metrics.SlackQueries.Load()
	if after != before+1 {
		t.Errorf("slack_queries_total: expected %d, got %d", before+1, after)
	}
}

func TestMetricsSlackReleaseIncrements(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// A release needs an active session window covering the release time.
	now := time.Now().UTC()
	_, err := testStore.DB().Exec(
		`INSERT INTO windows (kind, started_at, ends_at) VALUES (?, ?, ?)`,
		"session", store.FormatTime(now.Add(-time.Hour)), store.FormatTime(now.Add(time.Hour)),
	)
	if err != nil {
		t.Fatalf("failed to seed window: %v", err)
	}

	before := srv.metrics.SlackReleases.Load()

	payload := ReleaseRequest{
		ReleasedAt: now,
		JobTag:     "job-1",
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/slack/release", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	after := srv.metrics.SlackReleases.Load()
	if after != before+1 {
		t.Errorf("slack_releases_total: expected %d, got %d", before+1, after)
	}
}

func TestMetricsSlackReleaseFailureDoesNotIncrement(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	before := srv.metrics.SlackReleases.Load()

	// No active window — should 409 and not increment.
	payload := ReleaseRequest{
		ReleasedAt: time.Now().UTC(),
		JobTag:     "job-x",
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/slack/release", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	after := srv.metrics.SlackReleases.Load()
	if after != before {
		t.Errorf("counter should not increment on failed release: before=%d after=%d", before, after)
	}
}

func TestMetricsEndpointPrometheusOutput(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// Drive a few handlers to populate counters.
	srv.metrics.IncEventsIngested("api")
	srv.metrics.IncEventsIngested("api")
	srv.metrics.IncEventsIngested("tailer")
	srv.metrics.SnapshotsReceived.Add(3)
	srv.metrics.ParseErrors.Add(1)
	srv.metrics.SlackQueries.Add(7)
	srv.metrics.SlackReleases.Add(2)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected Content-Type text/plain..., got %q", ct)
	}

	body := w.Body.String()

	wantLines := []string{
		"# TYPE events_ingested_total counter",
		`events_ingested_total{source="api"} 2`,
		`events_ingested_total{source="tailer"} 1`,
		"# TYPE snapshots_received_total counter",
		"snapshots_received_total 3",
		"# TYPE parse_errors_total counter",
		"parse_errors_total 1",
		"# TYPE slack_queries_total counter",
		"slack_queries_total 7",
		"# TYPE slack_releases_total counter",
		"slack_releases_total 2",
	}
	for _, line := range wantLines {
		if !strings.Contains(body, line) {
			t.Errorf("metrics output missing line %q\nfull body:\n%s", line, body)
		}
	}
}

func TestMetricsEndpointEmpty(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	// Even with no events, the type/help lines and zero-valued counters should appear.
	for _, line := range []string{
		"snapshots_received_total 0",
		"parse_errors_total 0",
		"slack_queries_total 0",
		"slack_releases_total 0",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("expected zero-valued line %q in body:\n%s", line, body)
		}
	}
}
