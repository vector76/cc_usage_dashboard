// Package dashboard provides the web dashboard UI and JSON state endpoint.
package dashboard

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/slack"
	"github.com/vector76/cc_usage_dashboard/internal/store"
	"github.com/vector76/cc_usage_dashboard/internal/windows"
)

//go:embed static
var staticFS embed.FS

// WindowState describes one active window in the dashboard state response.
type WindowState struct {
	ID            int64     `json:"id"`
	Kind          string    `json:"kind"`
	StartedAt     time.Time `json:"started_at"`
	EndsAt        time.Time `json:"ends_at"`
	BaselineTotal *float64  `json:"baseline_total"`
	Consumed      float64   `json:"consumed"`
	Slack         *float64  `json:"slack"`
}

// SeriesBucket is a 15-minute consumption bucket.
type SeriesBucket struct {
	BucketStart time.Time `json:"bucket_start"`
	CostUSD     float64   `json:"cost_usd"`
}

// State is the JSON response for GET /api/dashboard/state.
type State struct {
	Now                    time.Time      `json:"now"`
	FiveHour               *WindowState   `json:"five_hour"`
	Weekly                 *WindowState   `json:"weekly"`
	LastSnapshotAgeSeconds *float64       `json:"last_snapshot_age_seconds"`
	ParseErrors24h         int64          `json:"parse_errors_24h"`
	Paused                 bool           `json:"paused"`
	Drift                  *float64       `json:"drift"`
	DriftAlert             bool           `json:"drift_alert"`
	ConsumptionSeries      []SeriesBucket `json:"consumption_series"`
}

// Handler serves the dashboard UI and state endpoint.
type Handler struct {
	store                  *store.Store
	slackCalc              *slack.Calculator
	windowsEngine          *windows.Engine
	baselineDriftThreshold float64
	now                    func() time.Time
	indexHTML              []byte
}

// NewHandler builds a Handler. baselineDriftThreshold is the
// Slack.BaselineDriftThreshold config value used to compute drift_alert.
func NewHandler(s *store.Store, sc *slack.Calculator, we *windows.Engine, baselineDriftThreshold float64) (*Handler, error) {
	html, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: failed to load index.html: %w", err)
	}
	return &Handler{
		store:                  s,
		slackCalc:              sc,
		windowsEngine:          we,
		baselineDriftThreshold: baselineDriftThreshold,
		now:                    func() time.Time { return time.Now().UTC() },
		indexHTML:              html,
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

	state := &State{
		Now:               now,
		ConsumptionSeries: []SeriesBucket{},
	}

	fiveHour, err := h.loadActiveWindow(db, "five_hour", now)
	if err != nil {
		return nil, fmt.Errorf("five_hour window: %w", err)
	}
	state.FiveHour = fiveHour

	weekly, err := h.loadActiveWindow(db, "weekly", now)
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

	if fiveHour != nil {
		drift, err := h.windowsEngine.Drift(fiveHour.ID)
		if err != nil {
			return nil, fmt.Errorf("drift: %w", err)
		}
		state.Drift = drift
		if drift != nil && fiveHour.BaselineTotal != nil && *fiveHour.BaselineTotal > 0 {
			limit := h.baselineDriftThreshold * *fiveHour.BaselineTotal
			if math.Abs(*drift) > limit {
				state.DriftAlert = true
			}
		}
	}

	series, err := h.consumptionSeries(db, now)
	if err != nil {
		return nil, fmt.Errorf("consumption series: %w", err)
	}
	state.ConsumptionSeries = series

	return state, nil
}

func (h *Handler) loadActiveWindow(db *sql.DB, kind string, now time.Time) (*WindowState, error) {
	var (
		id            int64
		startedAt     time.Time
		endsAt        time.Time
		baselineTotal sql.NullFloat64
	)
	err := db.QueryRow(`
		SELECT id, started_at, ends_at, baseline_total
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
		ws.BaselineTotal = &v
	}

	var consumed float64
	err = db.QueryRow(`
		SELECT COALESCE(SUM(cost_usd_equivalent), 0)
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at < ? AND cost_usd_equivalent IS NOT NULL
	`, store.FormatTime(startedAt), store.FormatTime(endsAt)).Scan(&consumed)
	if err != nil {
		return nil, err
	}
	ws.Consumed = consumed

	if ws.BaselineTotal != nil {
		duration := endsAt.Sub(startedAt)
		if duration > 0 {
			progress := float64(now.Sub(startedAt)) / float64(duration)
			if progress < 0 {
				progress = 0
			}
			if progress > 1 {
				progress = 1
			}
			expected := *ws.BaselineTotal * progress
			slk := expected - consumed
			ws.Slack = &slk
		}
	}

	return ws, nil
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

func (h *Handler) consumptionSeries(db *sql.DB, now time.Time) ([]SeriesBucket, error) {
	cutoff := now.Add(-24 * time.Hour)
	rows, err := db.Query(`
		SELECT
			CAST(strftime('%s', occurred_at) AS INTEGER) / 900 * 900 AS bucket_unix,
			COALESCE(SUM(cost_usd_equivalent), 0) AS total
		FROM usage_events
		WHERE occurred_at >= ? AND cost_usd_equivalent IS NOT NULL
		GROUP BY bucket_unix
		ORDER BY bucket_unix
	`, store.FormatTime(cutoff))
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
