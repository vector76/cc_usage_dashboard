package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/vector76/cc_usage_dashboard/internal/consumption"
)

// handleUsageBreakdown serves GET /api/usage/breakdown, the per-model token and
// cost report for an arbitrary time range.
//
// Range parameters (see consumption.Calculator.ParseRange for the precedence
// rules): ?start= and ?end= as RFC3339 instants, or ?period=24h|7d|30d as
// shorthand for a window ending now. Nothing at all means the last 24h.
//
// A malformed or inverted range is the caller's error, so it answers 400 —
// distinct from the 500 a query failure produces. Neither response echoes the
// submitted parameters: the value is logged so debugging stays possible, but the
// client already knows what it sent, and reflecting it back buys nothing.
func (s *Server) handleUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	startStr, endStr, period := q.Get("start"), q.Get("end"), q.Get("period")

	calc := consumption.NewCalculator(s.store.DB())
	start, end, err := calc.ParseRange(startStr, endStr, period)
	if err != nil {
		slog.Warn("usage breakdown: bad range",
			"err", err, "start", startStr, "end", endStr, "period", period)
		writeJSONError(w, http.StatusBadRequest, "invalid time range")
		return
	}

	result, err := calc.Breakdown(start, end)
	if err != nil {
		slog.Error("usage breakdown failed", "err", err, "start", start, "end", end)
		writeJSONError(w, http.StatusInternalServerError, "breakdown error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}
