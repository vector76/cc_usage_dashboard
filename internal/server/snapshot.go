package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// SnapshotRequest represents the POST /snapshot payload.
// SessionUsed and WeeklyUsed are 0–100 percentages scraped from the
// claude.ai usage page (the "Current session" and "All models" rows).
type SnapshotRequest struct {
	ObservedAt        time.Time  `json:"observed_at"`
	Source            string     `json:"source"`
	SessionUsed       *float64   `json:"session_used"`
	SessionWindowEnds *time.Time `json:"session_window_ends"`
	WeeklyUsed        *float64   `json:"weekly_used"`
	WeeklyWindowEnds  *time.Time `json:"weekly_window_ends"`
	RawDOMText        string     `json:"raw_dom_text,omitempty"`
}

// Sanity bounds for snapshot timestamps. Anything outside these ranges is
// almost certainly a bug, a clock skew, or a malicious payload trying to
// derail windowing math (the engine treats *_window_ends as the canonical
// reset boundary; a year-9999 value would freeze a window forever).
//
// Session windows are 5h, weekly windows are 7d. The fudge factors absorb
// snapshot-to-fire latency and small clock drift; ObservedAt's wider past
// bound accommodates the userscript's "Last updated: N minutes ago"
// staleness adjustment.
const (
	maxSessionEndsFuture = 6 * time.Hour
	maxWeeklyEndsFuture  = 8 * 24 * time.Hour
	maxEndsPast          = 1 * time.Hour
	maxObservedPast      = 24 * time.Hour
	maxObservedFuture    = 1 * time.Hour
)

// handleSnapshot processes POST /snapshot requests.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPOST(w, r, maxBodySnapshot) {
		return
	}

	var req SnapshotRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if err := validateSnapshotTimestamps(&req, s.now()); err != nil {
		slog.Warn("rejecting snapshot with out-of-range timestamp", "err", err, "source", req.Source)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Store the raw JSON for forensic recovery
	rawJSON, _ := json.Marshal(req)

	// Insert snapshot
	id, err := s.store.InsertQuotaSnapshot(
		req.ObservedAt,
		time.Now(),
		req.Source,
		req.SessionUsed,
		req.SessionWindowEnds,
		req.WeeklyUsed,
		req.WeeklyWindowEnds,
		string(rawJSON),
	)

	if err != nil {
		slog.Error("failed to insert quota snapshot", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "database error")
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
	if err := s.windowsEngine.UpdateWindows(); err != nil {
		slog.Error("failed to update windows", "err", err)
	}
}

// validateSnapshotTimestamps rejects snapshots whose ObservedAt or
// *_window_ends fields are wildly out of range. The window-ends fields
// drive the engine's reset-boundary math; an unbounded value here would
// freeze a window indefinitely (year 9999) or shove it into the deep
// past. Zero-valued timestamps are tolerated — the userscript may omit
// the reset hint when the source DOM doesn't expose one.
func validateSnapshotTimestamps(req *SnapshotRequest, now time.Time) error {
	if !req.ObservedAt.IsZero() {
		if req.ObservedAt.Before(now.Add(-maxObservedPast)) {
			return fmt.Errorf("observed_at too far in the past")
		}
		if req.ObservedAt.After(now.Add(maxObservedFuture)) {
			return fmt.Errorf("observed_at too far in the future")
		}
	}
	if req.SessionWindowEnds != nil {
		if req.SessionWindowEnds.Before(now.Add(-maxEndsPast)) {
			return fmt.Errorf("session_window_ends too far in the past")
		}
		if req.SessionWindowEnds.After(now.Add(maxSessionEndsFuture)) {
			return fmt.Errorf("session_window_ends too far in the future")
		}
	}
	if req.WeeklyWindowEnds != nil {
		if req.WeeklyWindowEnds.Before(now.Add(-maxEndsPast)) {
			return fmt.Errorf("weekly_window_ends too far in the past")
		}
		if req.WeeklyWindowEnds.After(now.Add(maxWeeklyEndsFuture)) {
			return fmt.Errorf("weekly_window_ends too far in the future")
		}
	}
	return nil
}

