package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/slack"
)

// ReleaseRequest represents a POST /slack/release payload.
type ReleaseRequest struct {
	ReleasedAt     time.Time `json:"released_at"`
	JobTag         string    `json:"job_tag"`
	EstimatedCost  *float64  `json:"estimated_cost,omitempty"`
	SlackAtRelease *float64  `json:"slack_at_release,omitempty"`
	WindowKind     string    `json:"window_kind,omitempty"`
}

// handleSlackQuery processes GET /slack requests.
func (s *Server) handleSlackQuery(w http.ResponseWriter, r *http.Request) {
	s.metrics.SlackQueries.Add(1)

	slackResp, err := s.slackCalc.GetSlack()
	if err != nil {
		slog.Error("failed to compute slack", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "computation error")
		return
	}

	if s.cfg.EnableSlackSampling && slackResp.SlackCombinedFraction != nil {
		_, err := s.slackCalc.RecordSample(slackResp.SlackCombinedFraction)
		if err != nil {
			slog.Warn("failed to record slack sample", "err", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(slackResp)
}

// handleSlackRelease processes POST /slack/release requests.
func (s *Server) handleSlackRelease(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPOST(w, r, maxBodySlackRelease) {
		return
	}

	var req ReleaseRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.JobTag == "" {
		writeJSONError(w, http.StatusBadRequest, "job_tag required")
		return
	}
	if req.ReleasedAt.IsZero() {
		writeJSONError(w, http.StatusBadRequest, "released_at required")
		return
	}

	windowKind := req.WindowKind
	if windowKind == "" {
		windowKind = "session"
	}
	if windowKind != "session" && windowKind != "weekly" {
		writeJSONError(w, http.StatusBadRequest, "invalid window_kind")
		return
	}

	id, err := s.slackCalc.RecordRelease(req.ReleasedAt, req.JobTag, req.EstimatedCost, req.SlackAtRelease, windowKind)
	if err != nil {
		if errors.Is(err, slack.ErrNoActiveWindow) {
			writeJSONError(w, http.StatusConflict, "no active window")
			return
		}
		slog.Error("failed to record release", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "database error")
		return
	}

	s.metrics.SlackReleases.Add(1)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id,
	})
}
