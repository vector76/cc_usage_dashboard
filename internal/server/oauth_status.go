package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/oauthusage"
)

// OAuthStatusSource reports the OAuth usage poller's health. Satisfied by
// *oauthusage.Poller; an interface so handler tests need no credentials.
type OAuthStatusSource interface {
	Status() oauthusage.Status
}

// OAuthStatusResponse is the body of GET /api/oauth/status.
//
// It exists because the other signals cannot answer "is OAuth polling
// working?": snapshots_received_total and the snapshot age count every
// source, so a running userscript masks a broken poller.
//
// Times are UTC, and null until first set, never Go's zero time, which a caller
// would read as a very old reading. last_success survives later failures,
// so the gap between it and last_attempt is how long the source has been
// down.
type OAuthStatusResponse struct {
	Enabled         bool       `json:"enabled"`
	Available       bool       `json:"available"`
	CredentialStale bool       `json:"credential_stale"`
	LastAttempt     *time.Time `json:"last_attempt"`
	LastSuccess     *time.Time `json:"last_success"`
	LastError       string     `json:"last_error"`
}

// SetOAuthStatus attaches the poller whose health GET /api/oauth/status
// reports. Left unset when oauth_usage is disabled, and the endpoint then
// answers enabled=false. Safe to call once before serving traffic;
// concurrent calls are not supported.
func (s *Server) SetOAuthStatus(src OAuthStatusSource) {
	s.oauthStatus = src
}

// handleOAuthStatus serves GET /api/oauth/status.
func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	resp := OAuthStatusResponse{}
	if s.oauthStatus != nil {
		st := s.oauthStatus.Status()
		resp = OAuthStatusResponse{
			Enabled:         true,
			Available:       st.Available,
			CredentialStale: st.CredentialStale,
			LastAttempt:     timeOrNil(st.LastAttempt),
			LastSuccess:     timeOrNil(st.LastSuccess),
			LastError:       st.LastError,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// timeOrNil maps an unset time to null and reports the rest in UTC: the
// poller stamps local wall-clock times, and /slack reports UTC.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
