// Package server provides the HTTP server for the trayapp.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/anthropics/usage-dashboard/internal/config"
	"github.com/anthropics/usage-dashboard/internal/dashboard"
	"github.com/anthropics/usage-dashboard/internal/ingest"
	"github.com/anthropics/usage-dashboard/internal/slack"
	"github.com/anthropics/usage-dashboard/internal/store"
	"github.com/anthropics/usage-dashboard/internal/windows"
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
}

// New creates a new HTTP server.
func New(s *store.Store, cfg *config.Config) *Server {
	srv := &Server{
		mux:        http.NewServeMux(),
		store:      s,
		cfg:        cfg,
		priceTable: ingest.LoadPriceTable(cfg.Pricing.TablePath),
		metrics:    NewMetrics(),
		slackCalc: slack.NewCalculator(s.DB(), slack.Config{
			QuietPeriodSeconds:     cfg.Slack.QuietPeriodSeconds,
			ReleaseThreshold:       cfg.Slack.ReleaseThreshold,
			BaselineMaxAgeHours:    cfg.Slack.BaselineMaxAgeHours,
			BaselineDriftThreshold: cfg.Slack.BaselineDriftThreshold,
		}),
		windowsEngine: windows.NewEngine(s.DB()),
	}

	dh, err := dashboard.NewHandler(s, srv.slackCalc, srv.windowsEngine, cfg.Slack.BaselineDriftThreshold)
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
	srv.mux.HandleFunc("GET /discount", srv.handleDiscount)
	srv.mux.HandleFunc("GET /metrics", srv.handleMetrics)
	if srv.dashboardHandler != nil {
		srv.dashboardHandler.Register(srv.mux)
	}

	return srv
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// SetTailer attaches a TailerStatus source whose CaughtUp() value is reported
// in /healthz. Safe to call once before serving traffic; concurrent calls are
// not supported.
func (s *Server) SetTailer(t TailerStatus) {
	s.tailerStatus = t
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

// LogRequest logs incoming requests (middleware-style).
func (s *Server) LogRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		duration := time.Since(start)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", duration.Milliseconds())
	})
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
		http.Error(w, `{"status":"unhealthy","reason":"database_not_writable"}`, http.StatusServiceUnavailable)
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
type LogPostRequest struct {
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int     `json:"cache_read_tokens,omitempty"`
	CostUSD             *float64 `json:"cost_usd,omitempty"`
	SessionID           string  `json:"session_id,omitempty"`
	MessageID           string  `json:"message_id,omitempty"`
	Model               string  `json:"model,omitempty"`
	ProjectPath         string  `json:"project_path,omitempty"`
	Source              string  `json:"source,omitempty"`
	RawJSON             string  `json:"raw_json,omitempty"`
}

// handleLog processes POST /log requests.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LogPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		return
	}

	// Validate required fields
	if req.InputTokens == 0 && req.OutputTokens == 0 {
		http.Error(w, `{"error":"input_tokens and output_tokens required"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
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

	// Insert into database
	id, err := s.store.InsertUsageEvent(
		time.Now(),
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
		slog.Error("failed to insert usage event", "err", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
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

// ParseErrorRequest represents the POST /parse_error request payload.
type ParseErrorRequest struct {
	Source  string `json:"source"`
	Reason  string `json:"reason"`
	Payload string `json:"payload"`
}

// handleParseError processes POST /parse_error requests.
func (s *Server) handleParseError(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ParseErrorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
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
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
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
