package server

import (
	"encoding/json"
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
}

// handleSlackQuery processes GET /slack requests.
func (s *Server) handleSlackQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.metrics.SlackQueries.Add(1)

	// Create calculator
	calculator := slack.NewCalculator(s.store.DB(), slack.Config{
		HeadroomThreshold:    s.cfg.Slack.HeadroomThreshold,
		QuietPeriodSeconds:   s.cfg.Slack.QuietPeriodSeconds,
		FreshnessThresholdMs: s.cfg.Slack.FreshnessThresholdMs,
	})

	// Get slack
	slackResp, err := calculator.GetSlack()
	if err != nil {
		slog.Error("failed to compute slack", "err", err)
		http.Error(w, `{"error":"computation error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	// Record sample if enabled
	if s.cfg.EnableSlackSampling && slackResp.SlackFraction != nil {
		_, err := calculator.RecordSample(slackResp.SlackFraction)
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

	// Validate required fields
	if req.JobTag == "" {
		http.Error(w, `{"error":"job_tag required"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	// Record the release
	calculator := slack.NewCalculator(s.store.DB(), slack.Config{})
	id, err := calculator.RecordRelease(req.ReleasedAt, req.JobTag, req.EstimatedCost, req.SlackAtRelease)
	if err != nil {
		if err.Error() == "no active 5-hour window for this release" {
			http.Error(w, `{"error":"no active window"}`, http.StatusConflict)
		} else {
			slog.Error("failed to record release", "err", err)
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		}
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
