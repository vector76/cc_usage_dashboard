// Package server provides the HTTP server for the trayapp.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/anthropics/usage-dashboard/internal/config"
	"github.com/anthropics/usage-dashboard/internal/ingest"
	"github.com/anthropics/usage-dashboard/internal/store"
)

// Server handles HTTP requests.
type Server struct {
	mux   *http.ServeMux
	store *store.Store
	cfg   *config.Config
	priceTable ingest.PriceTable
}

// New creates a new HTTP server.
func New(s *store.Store, cfg *config.Config) *Server {
	srv := &Server{
		mux:        http.NewServeMux(),
		store:      s,
		cfg:        cfg,
		priceTable: ingest.LoadPriceTable(cfg.Pricing.TablePath),
	}

	// Register handlers
	srv.mux.HandleFunc("GET /healthz", srv.handleHealthz)
	srv.mux.HandleFunc("POST /log", srv.handleLog)
	srv.mux.HandleFunc("POST /parse_error", srv.handleParseError)
	srv.mux.HandleFunc("POST /snapshot", srv.handleSnapshot)

	return srv
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
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

// handleHealthz checks if the trayapp and database are healthy.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Check database accessibility
	err := s.store.DB().Ping()
	if err != nil {
		http.Error(w, `{"status":"unhealthy","reason":"database_not_writable"}`, http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id,
	})
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(addr string) error {
	slog.Info("starting HTTP server", "addr", addr)
	return http.ListenAndServe(addr, s)
}
