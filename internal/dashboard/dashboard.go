// Package dashboard provides the web dashboard UI and JSON state endpoint.
package dashboard

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/icon"
	"github.com/vector76/cc_usage_dashboard/internal/slack"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

//go:embed static
var staticFS embed.FS

// WindowState describes one active window in the dashboard state response.
type WindowState struct {
	ID                  int64             `json:"id"`
	Kind                string            `json:"kind"`
	StartedAt           time.Time         `json:"started_at"`
	EndsAt              time.Time         `json:"ends_at"`
	BaselinePercentUsed *float64          `json:"baseline_percent_used"`
	Series              []UsedSeriesPoint `json:"series"`
	Volume              []SeriesBucket    `json:"volume"`
	// BucketSecs is the width of each Volume bucket in seconds. The
	// client uses this to size bars by their actual time span instead
	// of by the count of populated buckets, since GROUP BY only emits
	// rows for buckets that have data.
	BucketSecs int `json:"bucket_secs"`
}

// UsedSeriesPoint is one observation of % used at a point in time, sourced
// from quota_snapshots within (or for the session chart, near) the window.
// WindowEnds is the snapshot's reported reset time for this kind, so the
// client can split the series into one polyline per window — without it,
// the 15h session view would draw a single jagged line spanning resets.
type UsedSeriesPoint struct {
	ObservedAt  time.Time  `json:"observed_at"`
	PercentUsed float64    `json:"percent_used"`
	WindowEnds  *time.Time `json:"window_ends,omitempty"`
}

// SeriesBucket is one bucket of summed consumption. Width is set by the
// caller (see bucketSecsForKind) and reported to the client via
// WindowState.BucketSecs. BucketStart is UTC-aligned to multiples of the
// bucket width; the leftmost bucket can therefore start slightly before
// the chart's domain when the window doesn't begin on a bucket boundary.
type SeriesBucket struct {
	BucketStart time.Time `json:"bucket_start"`
	CostUSD     float64   `json:"cost_usd"`
}

// State is the JSON response for GET /api/dashboard/state.
type State struct {
	Now                    time.Time    `json:"now"`
	Session                *WindowState `json:"session"`
	Weekly                 *WindowState `json:"weekly"`
	LastSnapshotAgeSeconds *float64     `json:"last_snapshot_age_seconds"`
	ParseErrors24h         int64        `json:"parse_errors_24h"`
	Paused                 bool         `json:"paused"`
	// SlackReleaseRecommended mirrors the /slack endpoint's
	// release_recommended bit so the dashboard status panel can show
	// "release: yes/no" without a separate HTTP poll. Null if the slack
	// calculator errors (rare; surfaced in logs).
	SlackReleaseRecommended *bool `json:"slack_release_recommended"`
}

// Handler serves the dashboard UI and state endpoint.
type Handler struct {
	store     *store.Store
	slackCalc *slack.Calculator
	now       func() time.Time
	indexHTML []byte
}

func NewHandler(s *store.Store, sc *slack.Calculator) (*Handler, error) {
	html, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: failed to load index.html: %w", err)
	}
	return &Handler{
		store:     s,
		slackCalc: sc,
		now:       func() time.Time { return time.Now().UTC() },
		indexHTML: html,
	}, nil
}

// SetNow injects a clock for tests.
func (h *Handler) SetNow(fn func() time.Time) {
	h.now = fn
}

// Register attaches dashboard routes to the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", h.handleIndex)
	mux.HandleFunc("GET /dashboard", h.handleIndex)
	mux.HandleFunc("GET /api/dashboard/state", h.handleState)
	mux.HandleFunc("GET /favicon.png", h.handleFavicon)
	mux.HandleFunc("GET /favicon.ico", h.handleFavicon)
}

func (h *Handler) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	w.Write(icon.PNG)
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(h.indexHTML)
}

func (h *Handler) handleState(w http.ResponseWriter, r *http.Request) {
	state, err := h.computeState()
	if err != nil {
		slog.Error("dashboard state computation failed", "err", err)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"state computation failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(state)
}

func (h *Handler) computeState() (*State, error) {
	now := h.now()
	db := h.store.DB()

	state := &State{Now: now}

	session, err := h.loadActiveWindow(db, "session")
	if err != nil {
		return nil, fmt.Errorf("session window: %w", err)
	}
	state.Session = session

	weekly, err := h.loadActiveWindow(db, "weekly")
	if err != nil {
		return nil, fmt.Errorf("weekly window: %w", err)
	}
	state.Weekly = weekly

	if age, ok, err := h.lastSnapshotAge(db, now); err != nil {
		return nil, fmt.Errorf("snapshot age: %w", err)
	} else if ok {
		ageSec := age.Seconds()
		state.LastSnapshotAgeSeconds = &ageSec
	}

	count, err := h.parseErrors24h(db, now)
	if err != nil {
		return nil, fmt.Errorf("parse errors: %w", err)
	}
	state.ParseErrors24h = count

	state.Paused = h.slackCalc.IsPaused()

	if slackResp, err := h.slackCalc.GetSlack(); err != nil {
		// Don't fail the entire state response over a slack-calc hiccup;
		// log and leave SlackReleaseRecommended nil so the UI shows "—".
		slog.Warn("slack signal unavailable for dashboard state", "err", err)
	} else {
		v := slackResp.ReleaseRecommended
		state.SlackReleaseRecommended = &v
	}

	return state, nil
}

func (h *Handler) loadActiveWindow(db *sql.DB, kind string) (*WindowState, error) {
	var (
		id            int64
		startedAt     time.Time
		endsAt        time.Time
		baselineTotal sql.NullFloat64
	)
	err := db.QueryRow(`
		SELECT id, started_at, ends_at, baseline_percent_used
		FROM windows
		WHERE kind = ? AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`, kind).Scan(&id, &startedAt, &endsAt, &baselineTotal)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	ws := &WindowState{
		ID:        id,
		Kind:      kind,
		StartedAt: startedAt,
		EndsAt:    endsAt,
	}
	if baselineTotal.Valid {
		v := baselineTotal.Float64
		ws.BaselinePercentUsed = &v
	}

	// The session chart shows 10h of pre-window history alongside the
	// current 5h window so the user can compare today's burn rate against
	// the prior two sessions. The wider domain is purely for the snapshot
	// curve and the volume bars; the pace line is current-window-only.
	seriesStart := startedAt
	if kind == "session" {
		seriesStart = startedAt.Add(-10 * time.Hour)
	}

	series, err := h.loadUsedSeries(db, kind, seriesStart, endsAt)
	if err != nil {
		return nil, err
	}
	ws.Series = series

	bucketSecs := bucketSecsForKind(kind)
	volume, err := h.loadVolumeSeries(db, seriesStart, endsAt, bucketSecs)
	if err != nil {
		return nil, err
	}
	ws.Volume = volume
	ws.BucketSecs = bucketSecs

	return ws, nil
}

// bucketSecsForKind picks a bucket width that yields a readable number of
// bars across the chart's domain. Session view spans 15h (current 5h + 10h
// pre-window history) → 15-min buckets ≈ 60 bars; weekly = 7 days / 6h = 28
// bars. Returned size aligns to UTC by virtue of strftime('%s').
func bucketSecsForKind(kind string) int {
	switch kind {
	case "weekly":
		return 6 * 3600
	default:
		return 15 * 60
	}
}

// loadVolumeSeries buckets dollar consumption inside a window for the
// volume bar chart that sits below the % remaining curve. Each row is the
// sum of cost_usd_equivalent for usage_events whose occurred_at falls in
// [bucket_start, bucket_start + bucketSecs).
func (h *Handler) loadVolumeSeries(db *sql.DB, startedAt, endsAt time.Time, bucketSecs int) ([]SeriesBucket, error) {
	if bucketSecs <= 0 {
		return []SeriesBucket{}, nil
	}
	query := fmt.Sprintf(`
		SELECT
			(CAST(strftime('%%s', occurred_at) AS INTEGER) / %d) * %d AS bucket_unix,
			COALESCE(SUM(cost_usd_equivalent), 0) AS total
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at < ? AND cost_usd_equivalent IS NOT NULL
		GROUP BY bucket_unix
		ORDER BY bucket_unix
	`, bucketSecs, bucketSecs)
	rows, err := db.Query(query, store.FormatTime(startedAt), store.FormatTime(endsAt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SeriesBucket{}
	for rows.Next() {
		var bucketUnix int64
		var total float64
		if err := rows.Scan(&bucketUnix, &total); err != nil {
			return nil, err
		}
		out = append(out, SeriesBucket{
			BucketStart: time.Unix(bucketUnix, 0).UTC(),
			CostUSD:     total,
		})
	}
	return out, rows.Err()
}

// loadUsedSeries returns the per-snapshot %used time series for a window.
// Reads session_used+session_window_ends or weekly_used+weekly_window_ends
// depending on kind. Empty slice when no matching snapshots exist; never
// returns nil. WindowEnds is included so the client can split the polyline
// at session resets when rendering the 15h session view.
func (h *Handler) loadUsedSeries(db *sql.DB, kind string, startedAt, endsAt time.Time) ([]UsedSeriesPoint, error) {
	var usedCol, endsCol string
	switch kind {
	case "session":
		usedCol, endsCol = "session_used", "session_window_ends"
	case "weekly":
		usedCol, endsCol = "weekly_used", "weekly_window_ends"
	default:
		return []UsedSeriesPoint{}, nil
	}

	query := fmt.Sprintf(`
		SELECT observed_at, %s, %s
		FROM quota_snapshots
		WHERE observed_at >= ? AND observed_at < ? AND %s IS NOT NULL
		ORDER BY observed_at
	`, usedCol, endsCol, usedCol)
	rows, err := db.Query(query, store.FormatTime(startedAt), store.FormatTime(endsAt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UsedSeriesPoint{}
	for rows.Next() {
		var p UsedSeriesPoint
		var ends sql.NullTime
		if err := rows.Scan(&p.ObservedAt, &p.PercentUsed, &ends); err != nil {
			return nil, err
		}
		if ends.Valid {
			t := ends.Time
			p.WindowEnds = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (h *Handler) lastSnapshotAge(db *sql.DB, now time.Time) (time.Duration, bool, error) {
	var receivedAt time.Time
	err := db.QueryRow(`
		SELECT received_at FROM quota_snapshots
		ORDER BY received_at DESC LIMIT 1
	`).Scan(&receivedAt)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	age := now.Sub(receivedAt)
	if age < 0 {
		age = 0
	}
	return age, true, nil
}

func (h *Handler) parseErrors24h(db *sql.DB, now time.Time) (int64, error) {
	cutoff := now.Add(-24 * time.Hour)
	var count int64
	err := db.QueryRow(`
		SELECT COUNT(*) FROM parse_errors WHERE occurred_at >= ?
	`, store.FormatTime(cutoff)).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

