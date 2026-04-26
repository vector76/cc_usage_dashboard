package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/anthropics/usage-dashboard/internal/discount"
)

// handleDiscount processes GET /discount requests.
func (s *Server) handleDiscount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get period from query string, default to 24h
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "24h"
	}

	// Calculate discount
	calc := discount.NewCalculator(s.store.DB(), s.cfg.Subscription.MonthlyUSD, s.cfg.Subscription.BillingCycleDays)
	result, err := calc.Calculate(period)

	if err != nil {
		slog.Error("failed to calculate discount", "err", err)
		http.Error(w, `{"error":"calculation error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}
