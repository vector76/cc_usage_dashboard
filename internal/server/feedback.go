package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/vector76/cc_usage_dashboard/internal/feedback"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// FeedbackResponse is the GET /api/feedback payload. It bundles the three
// operational-feedback sources — buffered warn+ log records, the unknown-model
// aggregate, and recent parse_errors rows — plus summary counts so the
// dashboard can render a compact badge without walking the arrays.
type FeedbackResponse struct {
	Warnings      []feedback.Record       `json:"warnings"`
	UnknownModels []feedback.UnknownModel `json:"unknown_models"`
	ParseErrors   []store.ParseError      `json:"parse_errors"`
	Summary       FeedbackSummary         `json:"summary"`
}

// FeedbackSummary carries at-a-glance counts for the dashboard badge.
type FeedbackSummary struct {
	// Warnings is the number of warn+ records currently buffered.
	Warnings int `json:"warnings"`
	// UnknownModels is the number of distinct models missing from the price
	// table (drives the "N unknown model(s)" badge text).
	UnknownModels int `json:"unknown_models"`
	// UnknownModelEvents is the total count of unpriced usage events across
	// all unknown models.
	UnknownModelEvents int64 `json:"unknown_model_events"`
	// ParseErrors is the number of recent parse_errors rows returned.
	ParseErrors int `json:"parse_errors"`
}

const feedbackParseErrorLimit = 50

// handleFeedback serves GET /api/feedback.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	warnings := s.feedbackBuffer.Recent()
	unknown := s.unknownModels.Snapshot()

	parseErrors, err := s.store.RecentParseErrors(feedbackParseErrorLimit)
	if err != nil {
		slog.Error("failed to load recent parse errors for feedback", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "database error")
		return
	}

	var totalEvents int64
	for _, u := range unknown {
		totalEvents += u.Count
	}

	resp := FeedbackResponse{
		Warnings:      warnings,
		UnknownModels: unknown,
		ParseErrors:   parseErrors,
		Summary: FeedbackSummary{
			Warnings:           len(warnings),
			UnknownModels:      len(unknown),
			UnknownModelEvents: totalEvents,
			ParseErrors:        len(parseErrors),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
