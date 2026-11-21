package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vector76/cc_usage_dashboard/internal/ingest"
)

// SetPriceTable may be called while the server is running (a price list
// fetched from GitHub); events posted afterwards use the new table.
func TestSetPriceTableAppliesToLaterEvents(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()
	srv.SetPriceTable(ingest.PriceTable{"claude-new-9": {InputRate: 2, OutputRate: 2}})

	body, _ := json.Marshal(LogPostRequest{
		InputTokens: 1_000_000, SessionID: "s", MessageID: "m", Model: "claude-new-9", Source: "api",
	})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, jsonPOST("/log", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var cost float64
	var source string
	if err := testStore.DB().QueryRow(`SELECT cost_usd_equivalent, cost_source FROM usage_events`).Scan(&cost, &source); err != nil {
		t.Fatalf("query: %v", err)
	}
	if source != "computed" || cost != 2 {
		t.Errorf("cost = %v (%s), want 2 (computed)", cost, source)
	}
}
