package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// insertBreakdownEvent writes one usage event for the endpoint tests. The
// per-model aggregation itself is covered in internal/consumption; these tests
// only need enough data to prove the handler wires through to it.
func insertBreakdownEvent(t *testing.T, s *store.Store, occurred time.Time, model string, cost *float64, costSource string) {
	t.Helper()
	id := model + occurred.Format(time.RFC3339Nano)
	_, err := s.InsertUsageEvent(
		occurred, "api",
		"sess-"+id, "msg-"+id,
		"", model,
		100, 20, 5, 900,
		cost, costSource, "{}",
	)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func ptrF(f float64) *float64 { return &f }

func getJSON(t *testing.T, srv *Server, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var got map[string]any
	if w.Body.Len() > 0 {
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return w.Code, got
}

func TestHandleUsageBreakdown_ResponseShape(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Now().UTC()
	insertBreakdownEvent(t, testStore, now.Add(-1*time.Hour), "claude-opus-4-5", ptrF(3.00), "computed")
	insertBreakdownEvent(t, testStore, now.Add(-2*time.Hour), "claude-brand-new", ptrF(9.00), "ceiling")

	code, got := getJSON(t, srv, "/api/usage/breakdown?period=24h")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%v)", code, got)
	}

	required := []string{
		"start", "end",
		"total_cost_usd", "measured_cost_usd", "estimated_cost_usd",
		"events_total", "events_without_cost", "models",
	}
	for _, k := range required {
		if _, ok := got[k]; !ok {
			t.Errorf("response missing field %q (got keys: %v)", k, mapKeys(got))
		}
	}

	if got["measured_cost_usd"] != 3.00 {
		t.Errorf("measured_cost_usd = %v, want 3", got["measured_cost_usd"])
	}
	if got["estimated_cost_usd"] != 9.00 {
		t.Errorf("estimated_cost_usd = %v, want 9", got["estimated_cost_usd"])
	}

	models, ok := got["models"].([]any)
	if !ok {
		t.Fatalf("models is %T, want an array", got["models"])
	}
	if len(models) != 2 {
		t.Fatalf("want 2 model rows, got %d", len(models))
	}
	// Biggest spender first — the ceiling-priced guess at $9 outranks the $3.
	first, _ := models[0].(map[string]any)
	if first["model"] != "claude-brand-new" {
		t.Errorf("first row = %v, want claude-brand-new (highest cost)", first["model"])
	}
	if first["estimated"] != true {
		t.Errorf("ceiling row estimated = %v, want true", first["estimated"])
	}
	for _, k := range []string{
		"family", "events", "events_without_cost",
		"input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
		"cost_usd", "cost_source", "estimated",
	} {
		if _, ok := first[k]; !ok {
			t.Errorf("model row missing field %q (got keys: %v)", k, mapKeys(first))
		}
	}
}

// An empty range must serialize models as [] so the client can iterate without
// a null guard.
func TestHandleUsageBreakdown_EmptyRangeYieldsEmptyArray(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	code, got := getJSON(t, srv, "/api/usage/breakdown?period=24h")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	models, ok := got["models"].([]any)
	if !ok {
		t.Fatalf("models is %T (%v), want an empty array", got["models"], got["models"])
	}
	if len(models) != 0 {
		t.Errorf("want 0 rows, got %d", len(models))
	}
}

func TestHandleUsageBreakdown_DefaultsToLast24h(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	code, got := getJSON(t, srv, "/api/usage/breakdown")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	start, err := time.Parse(time.RFC3339, got["start"].(string))
	if err != nil {
		t.Fatalf("parse start %v: %v", got["start"], err)
	}
	end, err := time.Parse(time.RFC3339, got["end"].(string))
	if err != nil {
		t.Fatalf("parse end %v: %v", got["end"], err)
	}
	span := end.Sub(start)
	if span < 23*time.Hour || span > 25*time.Hour {
		t.Errorf("default span = %v, want ~24h", span)
	}
}

func TestHandleUsageBreakdown_ExplicitRange(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// Half-open [start, end): the event at exactly end must be excluded.
	rangeStart := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	rangeEnd := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	insertBreakdownEvent(t, testStore, rangeStart, "in-range", ptrF(1.00), "computed")
	insertBreakdownEvent(t, testStore, rangeEnd, "out-of-range", ptrF(2.00), "computed")

	code, got := getJSON(t, srv,
		"/api/usage/breakdown?start=2026-07-20T00:00:00Z&end=2026-07-21T00:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%v)", code, got)
	}
	if got["total_cost_usd"] != 1.00 {
		t.Errorf("total_cost_usd = %v, want 1 (boundary event excluded)", got["total_cost_usd"])
	}
	models := got["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("want 1 model row, got %d: %v", len(models), models)
	}
}

// A bad range is the caller's mistake, not a server fault: 400, not 500.
func TestHandleUsageBreakdown_BadRangeIsClientError(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	cases := map[string]string{
		"malformed start":   "/api/usage/breakdown?start=yesterday",
		"malformed end":     "/api/usage/breakdown?start=2026-07-20T00:00:00Z&end=soon",
		"malformed period":  "/api/usage/breakdown?period=lots",
		"end before start":  "/api/usage/breakdown?start=2026-07-21T00:00:00Z&end=2026-07-20T00:00:00Z",
		"end without start": "/api/usage/breakdown?end=2026-07-21T00:00:00Z",
		"zero span":         "/api/usage/breakdown?start=2026-07-20T00:00:00Z&end=2026-07-20T00:00:00Z",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			code, got := getJSON(t, srv, path)
			if code != http.StatusBadRequest {
				t.Errorf("got %d, want 400 (body=%v)", code, got)
			}
			if _, ok := got["error"]; !ok {
				t.Errorf("400 body missing an \"error\" field: %v", got)
			}
		})
	}
}

// The error body must not echo the caller's input back, same discipline as
// handleConsumption — a reflected value is one parser bug away from being a
// vector, and the client has the value it sent already.
func TestHandleUsageBreakdown_ErrorDoesNotEchoInput(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	marker := "notatime"
	req := httptest.NewRequest("GET", "/api/usage/breakdown?start="+marker, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
	if strings.Contains(w.Body.String(), marker) {
		t.Errorf("error body echoes the caller's input: %s", w.Body.String())
	}
}
