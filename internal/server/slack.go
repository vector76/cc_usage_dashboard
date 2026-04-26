package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/anthropics/usage-dashboard/internal/slack"
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
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.metrics.SlackQueries.Add(1)

	slackResp, err := s.slackCalc.GetSlack()
	if err != nil {
		slog.Error("failed to compute slack", "err", err)
		http.Error(w, `{"error":"computation error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	if req.JobTag == "" {
		http.Error(w, `{"error":"job_tag required"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		return
	}
	if req.ReleasedAt.IsZero() {
		http.Error(w, `{"error":"released_at required"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	windowKind := req.WindowKind
	if windowKind == "" {
		windowKind = "five_hour"
	}
	if windowKind != "five_hour" && windowKind != "weekly" {
		http.Error(w, `{"error":"invalid window_kind"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	id, err := s.slackCalc.RecordRelease(req.ReleasedAt, req.JobTag, req.EstimatedCost, req.SlackAtRelease, windowKind)
	if err != nil {
		if errors.Is(err, slack.ErrNoActiveWindow) {
			http.Error(w, `{"error":"no active window"}`, http.StatusConflict)
			w.Header().Set("Content-Type", "application/json")
			return
		}
		slog.Error("failed to record release", "err", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	s.metrics.SlackReleases.Add(1)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id,
	})
}
