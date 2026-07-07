package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/feedback"
)

// newIsolatedFeedbackServer returns a test server wired to fresh feedback
// instances so tests don't contaminate (or get contaminated by) the
// process-wide defaults.
func newIsolatedFeedbackServer(t *testing.T) (*Server, *feedback.Buffer, *feedback.UnknownModels) {
	t.Helper()
	srv, testStore := createTestServer(t)
	t.Cleanup(func() { testStore.Close() })
	buf := feedback.NewBuffer(50)
	um := feedback.NewUnknownModels()
	srv.SetFeedback(buf, um)
	return srv, buf, um
}

func getFeedback(t *testing.T, srv *Server) (int, FeedbackResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/feedback", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var resp FeedbackResponse
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode feedback response: %v (body=%s)", err, w.Body.String())
		}
	}
	return w.Code, resp
}

func TestHandleFeedback_Empty(t *testing.T) {
	srv, _, _ := newIsolatedFeedbackServer(t)

	code, resp := getFeedback(t, srv)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	// Arrays must be present and empty (not null) so the frontend can iterate.
	if resp.Warnings == nil {
		t.Error("warnings must be non-nil")
	}
	if resp.UnknownModels == nil {
		t.Error("unknown_models must be non-nil")
	}
	if resp.ParseErrors == nil {
		t.Error("parse_errors must be non-nil")
	}
	if resp.Summary.Warnings != 0 || resp.Summary.UnknownModels != 0 ||
		resp.Summary.UnknownModelEvents != 0 || resp.Summary.ParseErrors != 0 {
		t.Errorf("expected all-zero summary, got %+v", resp.Summary)
	}
}

func TestHandleFeedback_JSONShapeAndContent(t *testing.T) {
	srv, buf, um := newIsolatedFeedbackServer(t)

	buf.Add(feedback.Record{
		Time: time.Now(), Level: "WARN", Message: "price table file not found",
		Attrs: map[string]string{"path": "/etc/prices.yaml"},
	})
	um.Record("mystery-model", time.Now())
	um.Record("mystery-model", time.Now())

	// Seed a parse error via the DB-backed store.
	if _, err := srv.store.InsertParseError(time.Now(), "tailer", "bad json", "payload"); err != nil {
		t.Fatalf("InsertParseError: %v", err)
	}

	code, resp := getFeedback(t, srv)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}

	if len(resp.Warnings) != 1 || resp.Warnings[0].Message != "price table file not found" {
		t.Errorf("unexpected warnings: %+v", resp.Warnings)
	}
	if len(resp.UnknownModels) != 1 {
		t.Fatalf("expected 1 unknown model, got %d", len(resp.UnknownModels))
	}
	if resp.UnknownModels[0].Model != "mystery-model" || resp.UnknownModels[0].Count != 2 {
		t.Errorf("unexpected unknown model: %+v", resp.UnknownModels[0])
	}
	if len(resp.ParseErrors) != 1 || resp.ParseErrors[0].Reason != "bad json" {
		t.Errorf("unexpected parse errors: %+v", resp.ParseErrors)
	}

	// Summary counts.
	if resp.Summary.Warnings != 1 {
		t.Errorf("summary.warnings: got %d, want 1", resp.Summary.Warnings)
	}
	if resp.Summary.UnknownModels != 1 {
		t.Errorf("summary.unknown_models: got %d, want 1", resp.Summary.UnknownModels)
	}
	if resp.Summary.UnknownModelEvents != 2 {
		t.Errorf("summary.unknown_model_events: got %d, want 2", resp.Summary.UnknownModelEvents)
	}
	if resp.Summary.ParseErrors != 1 {
		t.Errorf("summary.parse_errors: got %d, want 1", resp.Summary.ParseErrors)
	}
}

// TestHandleFeedback_LogPathFeedsUnknownModel verifies the HTTP /log ingest
// path records unpriced models into the same aggregate the feedback endpoint
// reads — the second of the two required ResolveCost call sites.
func TestHandleFeedback_LogPathFeedsUnknownModel(t *testing.T) {
	srv, _, _ := newIsolatedFeedbackServer(t)

	body := []byte(`{"input_tokens":100,"output_tokens":50,"model":"http-unpriced-model","session_id":"s","message_id":"m1"}`)
	req := jsonPOST("/log", body)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /log, got %d (body=%s)", w.Code, w.Body.String())
	}

	_, resp := getFeedback(t, srv)
	if len(resp.UnknownModels) != 1 || resp.UnknownModels[0].Model != "http-unpriced-model" {
		t.Fatalf("expected http-unpriced-model aggregated, got %+v", resp.UnknownModels)
	}
	if resp.UnknownModels[0].Count != 1 {
		t.Errorf("expected count 1, got %d", resp.UnknownModels[0].Count)
	}
}

// TestHandleFeedback_LogPathIgnoresEmptyModel ensures an event with no model
// (a different, already-handled case) is not aggregated as an unknown model.
func TestHandleFeedback_LogPathIgnoresEmptyModel(t *testing.T) {
	srv, _, _ := newIsolatedFeedbackServer(t)

	body := []byte(`{"input_tokens":100,"output_tokens":50,"session_id":"s","message_id":"m1"}`)
	req := jsonPOST("/log", body)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /log, got %d (body=%s)", w.Code, w.Body.String())
	}

	_, resp := getFeedback(t, srv)
	if len(resp.UnknownModels) != 0 {
		t.Errorf("empty model must not be aggregated, got %+v", resp.UnknownModels)
	}
}
