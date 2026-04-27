package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/vector76/cc_usage_dashboard/internal/consumption"
)

// handleConsumption processes GET /consumption?period=24h requests.
func (s *Server) handleConsumption(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "24h"
	}

	calc := consumption.NewCalculator(s.store.DB())
	result, err := calc.Calculate(period)
	if err != nil {
		// Most failures here are DB errors from the underlying queries;
		// a malformed ?period also lands here but is the rarer case. We
		// log the period (so debugging stays possible) but never echo it
		// in the response body — the dashboard renders d.period via
		// textContent regardless, but the layered defence is cheap.
		slog.Error("consumption calculation failed", "err", err, "period", period)
		writeJSONError(w, http.StatusInternalServerError, "calculation error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}
