package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleDiscount_ResponseShape(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	srv.cfg.Subscription.MonthlyUSD = 200
	srv.cfg.Subscription.BillingCycleDays = 30

	req := httptest.NewRequest("GET", "/discount?period=7d", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	required := []string{
		"period", "period_start", "period_end",
		"consumed_usd_equivalent", "subscription_cost_prorated_usd",
		"value_ratio", "discount_pct", "savings_usd",
		"events_total", "events_with_reported_cost",
		"events_with_computed_cost", "events_without_cost",
		"cost_coverage_pct",
	}
	for _, k := range required {
		if _, ok := got[k]; !ok {
			t.Errorf("response missing field %q (got keys: %v)", k, mapKeys(got))
		}
	}

	if got["period"] != "7d" {
		t.Errorf("period: got %v, want \"7d\"", got["period"])
	}

	// With no events, ratios must be null and savings must equal -S.
	if got["value_ratio"] != nil {
		t.Errorf("value_ratio: got %v, want null", got["value_ratio"])
	}
	if got["discount_pct"] != nil {
		t.Errorf("discount_pct: got %v, want null", got["discount_pct"])
	}
}

func TestHandleDiscount_DefaultPeriod(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	srv.cfg.Subscription.MonthlyUSD = 200
	srv.cfg.Subscription.BillingCycleDays = 30

	req := httptest.NewRequest("GET", "/discount", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["period"] != "24h" {
		t.Errorf("default period: got %v, want \"24h\"", got["period"])
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
