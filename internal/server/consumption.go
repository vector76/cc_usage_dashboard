package server

import (
	"encoding/json"
	"errors"
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
		// We log the period (so debugging stays possible) but never echo
		// it in the response body — the dashboard renders d.period via
		// textContent regardless, but the layered defence is cheap. A
		// malformed ?period is the caller's error, so it answers 400 like
		// /api/usage/breakdown; anything else is a query failure.
		if errors.Is(err, consumption.ErrInvalidPeriod) {
			slog.Warn("consumption: bad period", "err", err, "period", period)
			writeJSONError(w, http.StatusBadRequest, "invalid period")
			return
		}
		slog.Error("consumption calculation failed", "err", err, "period", period)
		writeJSONError(w, http.StatusInternalServerError, "calculation error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(result)
}
