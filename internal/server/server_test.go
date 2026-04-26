package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/usage-dashboard/internal/config"
	"github.com/anthropics/usage-dashboard/internal/store"
)

func createTestServer(t *testing.T) (*Server, *store.Store) {
	testStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := &config.Config{}
	cfg.HTTP.Port = 0 // Use any available port
	cfg.Database.Path = ":memory:"
	cfg.Pricing.TablePath = ""

	return New(testStore, cfg), testStore
}

func TestHandleHealthz(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var result map[string]string
	err := json.NewDecoder(w.Body).Decode(&result)
	if err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["status"] != "healthy" {
		t.Errorf("expected status 'healthy', got %s", result["status"])
	}
}

func TestHandleLogValid(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	payload := LogPostRequest{
		InputTokens:  1000,
		OutputTokens: 500,
		SessionID:    "session-123",
		MessageID:    "msg-456",
		Model:        "claude-3-5-sonnet-20241022",
		Source:       "api",
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)
	if _, ok := result["id"]; !ok {
		t.Error("response missing 'id' field")
	}
}

func TestHandleLogMissingRequired(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	payload := LogPostRequest{
		// Missing required fields
		SessionID: "session-123",
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestHandleLogDuplicate(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	payload := LogPostRequest{
		InputTokens:  1000,
		OutputTokens: 500,
		SessionID:    "session-123",
		MessageID:    "msg-456",
		Source:       "api",
	}

	body, _ := json.Marshal(payload)

	// First POST should succeed
	req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("first POST failed with status %d", w.Code)
	}

	// Second POST with same (session_id, message_id) should fail with UNIQUE constraint
	req = httptest.NewRequest("POST", "/log", bytes.NewReader(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected second POST to fail with 500, got %d", w.Code)
	}
}

func TestHandleParseError(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	payload := ParseErrorRequest{
		Source:  "tailer",
		Reason:  "malformed JSON",
		Payload: `{"bad": json}`,
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/parse_error", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)
	if _, ok := result["id"]; !ok {
		t.Error("response missing 'id' field")
	}
}

func TestHandleLogInvalidJSON(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	req := httptest.NewRequest("POST", "/log", bytes.NewReader([]byte(`invalid json`)))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestHandleLogWithCost(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	cost := 0.05
	payload := LogPostRequest{
		InputTokens:  1000,
		OutputTokens: 500,
		CostUSD:      &cost,
		SessionID:    "session-123",
		Source:       "api",
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Verify the cost was stored
	rows, err := testStore.DB().Query("SELECT cost_usd_equivalent, cost_source FROM usage_events LIMIT 1")
	if err != nil {
		t.Fatalf("failed to query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Error("no events found")
		return
	}

	var storedCost float64
	var costSource string
	if err := rows.Scan(&storedCost, &costSource); err != nil {
		t.Fatalf("failed to scan: %v", err)
	}

	if storedCost != 0.05 {
		t.Errorf("expected cost 0.05, got %f", storedCost)
	}
	if costSource != "reported" {
		t.Errorf("expected cost_source 'reported', got %s", costSource)
	}
}
