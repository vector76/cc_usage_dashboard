package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/config"
	"github.com/vector76/cc_usage_dashboard/internal/store"
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

	// New() provisions the windows engine alongside other dependencies.
	// Tests don't call SetAllowedHosts, so the Host check stays disabled
	// and httptest.NewRequest's default Host="example.com" is accepted.
	return New(testStore, cfg), testStore
}

// jsonPOST builds a POST request with Content-Type: application/json,
// matching what every legitimate caller (CLI, userscript, dashboard
// fetches) sends. After the CSRF mitigation, plain httptest.NewRequest
// posts are rejected because they have no Content-Type at all.
func jsonPOST(path string, body []byte) *http.Request {
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
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

	var result map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&result)
	if err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if status, _ := result["status"].(string); status != "healthy" {
		t.Errorf("expected status 'healthy', got %v", result["status"])
	}
	if _, ok := result["tailer_caught_up"]; !ok {
		t.Error("response missing 'tailer_caught_up' field")
	}
}

type fakeTailer struct{ caughtUp bool }

func (f fakeTailer) CaughtUp() bool { return f.caughtUp }

func TestHandleHealthzReportsTailerStatus(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	srv.SetTailer(fakeTailer{caughtUp: true})

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	caught, ok := result["tailer_caught_up"].(bool)
	if !ok || !caught {
		t.Errorf("expected tailer_caught_up=true, got %v", result["tailer_caught_up"])
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
	req := jsonPOST("/log", body)
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
	req := jsonPOST("/log", body)
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
	req := jsonPOST("/log", body)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("first POST failed with status %d", w.Code)
	}

	// Second POST with same (session_id, message_id) is the steady-state
	// case for the Stop hook re-walking the transcript: returns 200 with
	// {duplicate: true} rather than 500. The DB still ends up with one
	// row.
	req = jsonPOST("/log", body)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected duplicate POST to return 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if dup, _ := resp["duplicate"].(bool); !dup {
		t.Errorf("expected duplicate:true in response, got %v", resp)
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
	req := jsonPOST("/parse_error", body)
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

	req := jsonPOST("/log", []byte(`invalid json`))
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

// TestPOSTRejectsMissingContentType verifies the CSRF mitigation: without
// Content-Type: application/json the request is refused with 415, even if
// the body is valid JSON. Browsers cannot mount a "simple" cross-origin
// POST with this media type, so requiring it kills form-encoded CSRF.
func TestPOSTRejectsMissingContentType(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	body, _ := json.Marshal(LogPostRequest{InputTokens: 1, OutputTokens: 1})
	for _, path := range []string{"/log", "/snapshot", "/parse_error", "/slack/release"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("POST", path, bytes.NewReader(body))
			// No Content-Type header at all.
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("expected 415, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestPOSTRejectsWrongContentType verifies that browser-form CSRF media
// types are rejected even though the body parses as JSON.
func TestPOSTRejectsWrongContentType(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	body, _ := json.Marshal(LogPostRequest{InputTokens: 1, OutputTokens: 1})
	for _, ct := range []string{
		"text/plain",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
	} {
		t.Run(ct, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
			req.Header.Set("Content-Type", ct)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("expected 415, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestPOSTAcceptsJSONContentTypeWithCharset verifies that the parameterised
// form (which the userscript and most JSON clients send) is accepted.
func TestPOSTAcceptsJSONContentTypeWithCharset(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	body, _ := json.Marshal(LogPostRequest{InputTokens: 1, OutputTokens: 1, SessionID: "s", MessageID: "m"})
	req := httptest.NewRequest("POST", "/log", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestPOSTRejectsOversizeBody verifies that a body exceeding the per-
// endpoint limit yields 413, not OOM. Uses /slack/release which has the
// tightest limit (8 KiB) so the test stays fast.
func TestPOSTRejectsOversizeBody(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// Build a payload that is valid JSON but well over 8 KiB by padding
	// job_tag with a long string.
	huge := make([]byte, 16*1024)
	for i := range huge {
		huge[i] = 'a'
	}
	payload := map[string]any{
		"released_at": time.Now().UTC(),
		"job_tag":     string(huge),
	}
	body, _ := json.Marshal(payload)
	req := jsonPOST("/slack/release", body)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversize body, got %d (%s)", w.Code, w.Body.String())
	}
	var n int
	if err := testStore.DB().QueryRow(`SELECT COUNT(*) FROM slack_releases`).Scan(&n); err != nil {
		t.Fatalf("count slack_releases: %v", err)
	}
	if n != 0 {
		t.Errorf("oversize body must not insert; got %d rows", n)
	}
}

// TestHostAllowList verifies the DNS-rebinding defence: requests whose
// Host header is not in the configured allow-list are rejected with 403,
// regardless of method or path. Loopback Host values pass.
func TestHostAllowList(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// Configure the allow-list explicitly. This mirrors what the trayapp
	// main does after enumerating bind interfaces.
	srv.SetAllowedHosts([]string{"127.0.0.1", "172.17.0.1"}, 27812)

	allowed := []string{
		"127.0.0.1:27812",
		"localhost:27812",
		"host.docker.internal:27812",
		"172.17.0.1:27812",
		"127.0.0.1", // some clients strip the default port
		"localhost",
	}
	for _, h := range allowed {
		t.Run("allow:"+h, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/healthz", nil)
			req.Host = h
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("expected 200 for Host=%q, got %d", h, w.Code)
			}
		})
	}

	denied := []string{
		"attacker.example",
		"attacker.example:27812",
		"evil.com:80",
		"10.0.0.5:27812", // not in the configured allow-list
		"",
	}
	for _, h := range denied {
		t.Run("deny:"+h, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/healthz", nil)
			req.Host = h
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("expected 403 for Host=%q, got %d", h, w.Code)
			}
		})
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
	req := jsonPOST("/log", body)
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
