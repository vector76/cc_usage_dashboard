// Package server provides the HTTP server for the trayapp.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/config"
	"github.com/vector76/cc_usage_dashboard/internal/dashboard"
	"github.com/vector76/cc_usage_dashboard/internal/ingest"
	"github.com/vector76/cc_usage_dashboard/internal/slack"
	"github.com/vector76/cc_usage_dashboard/internal/store"
	"github.com/vector76/cc_usage_dashboard/internal/windows"
)

// Per-endpoint request body limits. Anything larger is rejected with 413
// before json.Decode pulls it into memory. Values are loose upper bounds
// — real payloads are dramatically smaller — but tight enough that a
// hostile caller (LAN, browser, CSRF) cannot exhaust RAM or fill the DB.
const (
	maxBodyLog          = 1 << 20 // 1 MiB: /log carries a transcript line which can include cached blob
	maxBodySnapshot     = 1 << 16 // 64 KiB: /snapshot is a few floats + timestamps
	maxBodyParseError   = 1 << 18 // 256 KiB: /parse_error carries fingerprint diagnostics
	maxBodySlackRelease = 1 << 13 // 8 KiB: /slack/release is a fixed-shape struct
)

// TailerStatus reports whether the tailer has caught up with all known
// transcript files. The server uses it for the /healthz response without
// taking a hard dependency on the ingest package's concrete Tailer type.
type TailerStatus interface {
	CaughtUp() bool
}

// Server handles HTTP requests.
type Server struct {
	mux   *http.ServeMux
	store *store.Store
	cfg   *config.Config
	priceTable ingest.PriceTable
	metrics *Metrics
	slackCalc *slack.Calculator
	windowsEngine *windows.Engine
	dashboardHandler *dashboard.Handler
	tailerStatus TailerStatus

	// httpServers tracks every *http.Server this Server is currently
	// running so Shutdown can drain them all. The trayapp binds to
	// multiple addresses (loopback + detected interfaces), so each
	// ListenAndServe/Serve call appends one entry.
	mu          sync.Mutex
	httpServers []*http.Server

	// allowedHosts is the set of acceptable Host header values for any
	// inbound request. Set via SetAllowedHosts; nil means "no host check
	// applied" (the in-process httptest path used by unit tests). The
	// production wiring in cmd/trayapp must call SetAllowedHosts so DNS
	// rebinding cannot smuggle requests in via a forged Host header.
	allowedHosts map[string]struct{}

	// now returns the wall clock used by request handlers that need to
	// validate timestamps relative to "now" (currently the snapshot
	// handler's *_window_ends bounds check). Defaults to time.Now;
	// tests with fixed-date fixtures override via SetNow so the bounds
	// move with the fixture instead of drifting against real time.
	now func() time.Time
}

// New creates a new HTTP server.
func New(s *store.Store, cfg *config.Config) *Server {
	priceTable, err := ingest.LoadPriceTable(cfg.Pricing.TablePath)
	if err != nil {
		slog.Warn("price table load failed; cost computation disabled", "err", err)
	}
	srv := &Server{
		mux:        http.NewServeMux(),
		store:      s,
		cfg:        cfg,
		priceTable: priceTable,
		metrics:    NewMetrics(),
		slackCalc: slack.NewCalculator(s.DB(), slack.Config{
			BaselineMaxAgeSeconds:    cfg.Slack.BaselineMaxAgeSeconds,
			SessionSurplusThreshold:  cfg.Slack.SessionSurplusThreshold,
			WeeklySurplusThreshold:   cfg.Slack.WeeklySurplusThreshold,
			SessionAbsoluteThreshold: cfg.Slack.SessionAbsoluteThreshold,
			WeeklyAbsoluteThreshold:  cfg.Slack.WeeklyAbsoluteThreshold,
		}),
		windowsEngine: windows.NewEngine(s.DB()),
		now:           time.Now,
	}

	dh, err := dashboard.NewHandler(s, srv.slackCalc)
	if err != nil {
		// Embedded asset failure is a build-time error in practice; fall back to
		// nil handler so the rest of the server still starts. The dashboard
		// routes simply won't be registered.
		slog.Error("failed to initialize dashboard handler", "err", err)
	} else {
		srv.dashboardHandler = dh
	}

	// Register handlers
	srv.mux.HandleFunc("GET /healthz", srv.handleHealthz)
	srv.mux.HandleFunc("POST /log", srv.handleLog)
	srv.mux.HandleFunc("POST /parse_error", srv.handleParseError)
	srv.mux.HandleFunc("POST /snapshot", srv.handleSnapshot)
	srv.mux.HandleFunc("GET /slack", srv.handleSlackQuery)
	srv.mux.HandleFunc("POST /slack/release", srv.handleSlackRelease)
	srv.mux.HandleFunc("GET /consumption", srv.handleConsumption)
	srv.mux.HandleFunc("GET /metrics", srv.handleMetrics)
	if srv.dashboardHandler != nil {
		srv.dashboardHandler.Register(srv.mux)
	}

	return srv
}

// ServeHTTP implements http.Handler. Every request first passes the Host
// header allow-list (DNS rebinding defence) before reaching the mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hostAllowed(r.Host) {
		slog.Warn("rejected request with disallowed Host",
			"host", r.Host, "remote", r.RemoteAddr, "path", r.URL.Path)
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// SetAllowedHosts configures the Host header allow-list. Production wiring
// must include every interface address the server binds to (with the
// configured port) plus the well-known names containers and the host
// browser use to reach it. A nil/empty map disables the check, which is
// the default state used by unit tests that exercise handlers via
// httptest.NewRequest (Host="example.com"). Safe to call once before
// ListenAndServe; concurrent calls are not supported.
func (s *Server) SetAllowedHosts(boundAddrs []string, port int) {
	set := make(map[string]struct{})
	add := func(host string) {
		if host == "" {
			return
		}
		set[host] = struct{}{}
		set[net.JoinHostPort(host, strconv.Itoa(port))] = struct{}{}
	}
	// Well-known names: the userscript and host CLI both use loopback;
	// containers reach the host via host.docker.internal which Docker
	// resolves to one of the bound interface IPs.
	for _, h := range []string{"localhost", "127.0.0.1", "host.docker.internal"} {
		add(h)
	}
	for _, a := range boundAddrs {
		add(a)
	}
	s.allowedHosts = set
}

// hostAllowed reports whether the request's Host header is in the
// configured allow-list. A nil allow-list (tests) means "no check".
func (s *Server) hostAllowed(host string) bool {
	if s.allowedHosts == nil {
		return true
	}
	_, ok := s.allowedHosts[host]
	return ok
}

// requireJSONPOST validates that an incoming POST is acceptable: the
// Content-Type media type must be application/json, and the body is
// capped at maxBytes. The Content-Type check is the CSRF defence —
// browsers cannot mount a "simple" cross-origin POST with this media
// type, so any caller that passes is either same-origin, the userscript
// (GM.xmlHttpRequest), or a non-browser client that explicitly opts in
// (the CLI sets it). Returns true when the request may proceed; on
// rejection the response is already written.
func requireJSONPOST(w http.ResponseWriter, r *http.Request, maxBytes int64) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "missing Content-Type")
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/json" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return true
}

// writeJSONError writes a JSON {"error": ...} body with the given status.
// Centralised so the Content-Type and body are guaranteed consistent;
// http.Error writes text/plain regardless of any later header set.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// decodeJSONBody reads r.Body into v and writes the appropriate HTTP error
// on failure: 413 when the request exceeded the MaxBytesReader limit set
// by requireJSONPOST, 400 for any other decode failure (malformed JSON,
// type mismatches, EOF on empty body). Returns true when v was populated
// successfully.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

// SetTailer attaches a TailerStatus source whose CaughtUp() value is reported
// in /healthz. Safe to call once before serving traffic; concurrent calls are
// not supported.
func (s *Server) SetTailer(t TailerStatus) {
	s.tailerStatus = t
}

// SetNow injects a clock for tests whose fixtures use a fixed timestamp.
// Production never calls this — handlers default to time.Now.
func (s *Server) SetNow(fn func() time.Time) {
	s.now = fn
}

// WindowsEngine returns the server's internal windows engine so callers (the
// trayapp main, integration tests) can drive periodic UpdateWindows ticks
// against the same instance the server uses.
func (s *Server) WindowsEngine() *windows.Engine {
	return s.windowsEngine
}

// SlackCalculator returns the shared slack calculator instance so callers
// (the tray UI) can mutate the in-memory pause state on the same instance
// that the HTTP handlers read from.
func (s *Server) SlackCalculator() *slack.Calculator {
	return s.slackCalc
}

// handleHealthz checks if the trayapp and database are healthy. Per the
// Phase 7 contract, a tailer that hasn't caught up does NOT degrade health to
// 503 — only an unwritable database does. The current tailer state is
// surfaced via the tailer_caught_up field so dashboards/operators can observe
// it without paging.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Check database accessibility
	err := s.store.DB().Ping()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "unhealthy",
			"reason": "database_not_writable",
		})
		return
	}

	caughtUp := false
	if s.tailerStatus != nil {
		caughtUp = s.tailerStatus.CaughtUp()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           "healthy",
		"tailer_caught_up": caughtUp,
	})
}

// LogPostRequest represents the POST /log request payload.
//
// OccurredAt is when the assistant turn actually happened. The Stop hook
// reads it from each transcript line's "timestamp" field; manual /log
// callers can omit it and the server falls back to time.Now() at insert
// time. Without this, backfills compress all of a session's events into
// the few seconds the CLI takes to walk the transcript, which destroys
// the consumption-over-time signal the dashboard charts depend on.
type LogPostRequest struct {
	OccurredAt          *time.Time `json:"occurred_at,omitempty"`
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int        `json:"cache_read_tokens,omitempty"`
	CostUSD             *float64   `json:"cost_usd,omitempty"`
	SessionID           string     `json:"session_id,omitempty"`
	MessageID           string     `json:"message_id,omitempty"`
	Model               string     `json:"model,omitempty"`
	ProjectPath         string     `json:"project_path,omitempty"`
	Source              string     `json:"source,omitempty"`
	RawJSON             string     `json:"raw_json,omitempty"`
}

// handleLog processes POST /log requests.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPOST(w, r, maxBodyLog) {
		return
	}

	var req LogPostRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Validate required fields
	if req.InputTokens == 0 && req.OutputTokens == 0 {
		writeJSONError(w, http.StatusBadRequest, "input_tokens and output_tokens required")
		return
	}

	// Resolve cost
	cost, costSource := ingest.ResolveCost(
		req.CostUSD,
		req.Model,
		req.InputTokens,
		req.OutputTokens,
		req.CacheCreationTokens,
		req.CacheReadTokens,
		s.priceTable,
	)

	// Default source if not provided
	if req.Source == "" {
		req.Source = "api"
	}

	// Honor the request's occurred_at when supplied (Stop hook backfill).
	// Falls back to time.Now() for manual /log callers that don't set it.
	occurredAt := time.Now()
	if req.OccurredAt != nil {
		occurredAt = *req.OccurredAt
	}

	// Insert into database
	id, err := s.store.InsertUsageEvent(
		occurredAt,
		req.Source,
		req.SessionID,
		req.MessageID,
		req.ProjectPath,
		req.Model,
		req.InputTokens,
		req.OutputTokens,
		req.CacheCreationTokens,
		req.CacheReadTokens,
		cost,
		costSource,
		req.RawJSON,
	)

	if err != nil {
		// UNIQUE-constraint violations are the expected steady state when
		// the Stop hook re-walks the transcript: every assistant turn
		// already in the DB will collide on (session_id, message_id).
		// This is the idempotency mechanism — log it at debug, return
		// 200 with a duplicate flag, and don't bump the ingested metric.
		if isUniqueConstraintViolation(err) {
			slog.Debug("usage event already present (idempotent re-post)",
				"session_id", req.SessionID, "message_id", req.MessageID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"duplicate": true,
			})
			return
		}
		slog.Error("failed to insert usage event", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "database error")
		return
	}

	s.metrics.IncEventsIngested(req.Source)

	s.deriveWindows()

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id,
	})
}

// isUniqueConstraintViolation matches the modernc.org/sqlite error message
// for SQLITE_CONSTRAINT_UNIQUE (extended code 2067). The driver doesn't
// expose a sentinel error, so we string-match — narrow enough to catch
// only this case and not other constraint failures.
func isUniqueConstraintViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ParseErrorRequest represents the POST /parse_error request payload.
type ParseErrorRequest struct {
	Source  string `json:"source"`
	Reason  string `json:"reason"`
	Payload string `json:"payload"`
}

// handleParseError processes POST /parse_error requests.
func (s *Server) handleParseError(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPOST(w, r, maxBodyParseError) {
		return
	}

	var req ParseErrorRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	id, err := s.store.InsertParseError(
		time.Now(),
		req.Source,
		req.Reason,
		req.Payload,
	)

	if err != nil {
		slog.Error("failed to insert parse error", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "database error")
		return
	}

	s.metrics.ParseErrors.Add(1)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id,
	})
}

// ListenAndServe starts the HTTP server bound to addr. The underlying
// *http.Server is recorded so Shutdown(ctx) can drain it. Returns
// http.ErrServerClosed when Shutdown completes cleanly.
func (s *Server) ListenAndServe(addr string) error {
	slog.Info("starting HTTP server", "addr", addr)
	hs := &http.Server{Addr: addr, Handler: s}
	s.registerHTTPServer(hs)
	return hs.ListenAndServe()
}

// Serve runs the HTTP server on a pre-bound listener. Used by tests that
// need to discover the actual port (Listen with :0). The underlying
// *http.Server is recorded so Shutdown(ctx) can drain it.
func (s *Server) Serve(ln net.Listener) error {
	hs := &http.Server{Handler: s}
	s.registerHTTPServer(hs)
	return hs.Serve(ln)
}

func (s *Server) registerHTTPServer(hs *http.Server) {
	s.mu.Lock()
	s.httpServers = append(s.httpServers, hs)
	s.mu.Unlock()
}

// Shutdown gracefully stops all registered HTTP servers, allowing
// in-flight requests to complete until ctx is cancelled. Servers are
// drained concurrently so the worst-case wait is the slowest server,
// not the sum across servers (the trayapp binds to multiple
// interfaces). Returns the first non-nil error encountered.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	servers := append([]*http.Server(nil), s.httpServers...)
	s.mu.Unlock()

	errs := make(chan error, len(servers))
	for _, hs := range servers {
		go func(hs *http.Server) {
			errs <- hs.Shutdown(ctx)
		}(hs)
	}

	var firstErr error
	for range servers {
		if err := <-errs; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
