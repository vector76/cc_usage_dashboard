package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
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
	// FableWeeklyUsed is the "Fable" sub-row under the Weekly limits
	// heading, 0–100. Nil when absent, which covers both an older
	// userscript and a page that doesn't render the row (the sub-row is
	// plan-dependent). There is no fable_window_ends: the page reports the
	// same reset time for the Fable row as for the weekly aggregate, so
	// the series is anchored on the existing weekly window.
	FableWeeklyUsed *float64 `json:"fable_weekly_used,omitempty"`
	// Pointer so an absent field (NULL) is distinguishable from explicit false.
	SessionActive      *bool  `json:"session_active,omitempty"`
	WeeklyActive       *bool  `json:"weekly_active,omitempty"`
	ContinuousWithPrev *bool  `json:"continuous_with_prev,omitempty"`
	RawDOMText         string `json:"raw_dom_text,omitempty"`
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

	id, err := s.RecordSnapshot(req)
	if err != nil {
		if errors.Is(err, ErrInvalidSnapshot) {
			slog.Warn("rejecting snapshot with out-of-range timestamp", "err", err, "source", req.Source)
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("failed to insert quota snapshot", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "database error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id,
	})
}

// ErrInvalidSnapshot marks a snapshot rejected on its own contents rather
// than on a storage failure, so an HTTP caller can map it to 400 and an
// in-process caller can tell a bad reading from a broken database.
var ErrInvalidSnapshot = errors.New("invalid snapshot")

// RecordSnapshot validates, stores, and derives windows for one snapshot.
//
// This is the single path every quota source takes. The userscript reaches
// it through POST /snapshot; the OAuth usage poller calls it directly,
// in-process. Routing both through here is what keeps timestamp
// validation, raw-JSON retention, metrics and window derivation from
// drifting apart between the two — and what makes their rows directly
// comparable, since they differ only in Source.
func (s *Server) RecordSnapshot(req SnapshotRequest) (int64, error) {
	if err := validateSnapshotTimestamps(&req, s.now()); err != nil {
		return 0, fmt.Errorf("%w: %s", ErrInvalidSnapshot, err)
	}

	// Store the raw JSON for forensic recovery
	rawJSON, _ := json.Marshal(req)

	id, err := s.store.InsertQuotaSnapshotRecord(store.QuotaSnapshotRecord{
		ObservedAt:         req.ObservedAt,
		ReceivedAt:         time.Now(),
		Source:             req.Source,
		SessionUsed:        req.SessionUsed,
		SessionWindowEnds:  req.SessionWindowEnds,
		WeeklyUsed:         req.WeeklyUsed,
		WeeklyWindowEnds:   req.WeeklyWindowEnds,
		FableWeeklyUsed:    req.FableWeeklyUsed,
		SessionActive:      req.SessionActive,
		WeeklyActive:       req.WeeklyActive,
		ContinuousWithPrev: req.ContinuousWithPrev,
		RawJSON:            string(rawJSON),
	})
	if err != nil {
		return 0, err
	}

	s.metrics.SnapshotsReceived.Add(1)
	s.deriveWindows()

	return id, nil
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
