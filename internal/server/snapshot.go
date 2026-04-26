package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// SnapshotRequest represents the POST /snapshot payload.
type SnapshotRequest struct {
	ObservedAt           time.Time  `json:"observed_at"`
	Source               string     `json:"source"`
	FiveHourRemaining    *float64   `json:"five_hour_remaining"`
	FiveHourTotal        *float64   `json:"five_hour_total"`
	FiveHourWindowEnds   *time.Time `json:"five_hour_window_ends"`
	WeeklyRemaining      *float64   `json:"weekly_remaining"`
	WeeklyTotal          *float64   `json:"weekly_total"`
	WeeklyWindowEnds     *time.Time `json:"weekly_window_ends"`
	RawDOMText           string     `json:"raw_dom_text,omitempty"`
}

// handleSnapshot processes POST /snapshot requests.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("invalid snapshot payload", "err", err)
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	// Store the raw JSON for forensic recovery
	rawJSON, _ := json.Marshal(req)

	// Insert snapshot
	id, err := s.store.InsertQuotaSnapshot(
		req.ObservedAt,
		time.Now(),
		req.Source,
		req.FiveHourRemaining,
		req.FiveHourTotal,
		req.FiveHourWindowEnds,
		req.WeeklyRemaining,
		req.WeeklyTotal,
		req.WeeklyWindowEnds,
		string(rawJSON),
	)

	if err != nil {
		slog.Error("failed to insert quota snapshot", "err", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	s.metrics.SnapshotsReceived.Add(1)

	// Trigger windows derivation
	// In production, this would happen in a background task
	// For now, we'll do it synchronously
	s.deriveWindows()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id,
	})
}

// deriveWindows maintains the windows table based on events and snapshots.
// This is called after each snapshot or event insertion.
func (s *Server) deriveWindows() {
	// Placeholder: will be implemented in next iteration
	// For now, this is a no-op
}

