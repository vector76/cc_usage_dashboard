package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/vector76/cc_usage_dashboard/internal/consumption"
)

// handleConsumption processes GET /consumption?period=24h requests.
func (s *Server) handleConsumption(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	period := r.URL.Query().Get("period")
	if period == "" {
		period = "24h"
	}

	calc := consumption.NewCalculator(s.store.DB())
	result, err := calc.Calculate(period)
	if err != nil {
		slog.Error("failed to calculate consumption", "err", err)
		http.Error(w, `{"error":"calculation error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}
