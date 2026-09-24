package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/oauthusage"
)

// fakeOAuthStatus stands in for *oauthusage.Poller so the handler can be
// driven through every health state without credentials or a network.
type fakeOAuthStatus struct{ status oauthusage.Status }

func (f fakeOAuthStatus) Status() oauthusage.Status { return f.status }

func getOAuthStatus(t *testing.T, srv *Server) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/oauth/status", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode oauth status: %v", err)
	}
	return w.Code, body
}

func newOAuthStatusServer(t *testing.T) *Server {
	t.Helper()
	srv, testStore := createTestServer(t)
	t.Cleanup(func() { testStore.Close() })
	return srv
}

// With polling off there is no source to report on. That is a normal
// configuration, not an error, so it answers 200 and says so rather than
// leaving the caller to guess from a 404.
func TestHandleOAuthStatus_Disabled(t *testing.T) {
	srv := newOAuthStatusServer(t)

	code, body := getOAuthStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if body["available"] != false {
		t.Errorf("available = %v, want false", body["available"])
	}
	for _, k := range []string{"last_attempt", "last_success"} {
		if v, ok := body[k]; !ok || v != nil {
			t.Errorf("%s = %v (present=%v), want null", k, v, ok)
		}
	}
}

// Enabled but not yet polled: times have never been set, and must read as
// null rather than as Go's zero time (0001-01-01), which a caller would
// otherwise take for a very old reading.
func TestHandleOAuthStatus_BeforeFirstPoll(t *testing.T) {
	srv := newOAuthStatusServer(t)
	srv.SetOAuthStatus(fakeOAuthStatus{})

	code, body := getOAuthStatus(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["enabled"] != true {
		t.Errorf("enabled = %v, want true", body["enabled"])
	}
	if body["available"] != false {
		t.Errorf("available = %v, want false", body["available"])
	}
	for _, k := range []string{"last_attempt", "last_success"} {
		if v, ok := body[k]; !ok || v != nil {
			t.Errorf("%s = %v (present=%v), want null", k, v, ok)
		}
	}
	if body["last_error"] != "" {
		t.Errorf("last_error = %v, want empty", body["last_error"])
	}
}

func TestHandleOAuthStatus_Healthy(t *testing.T) {
	srv := newOAuthStatusServer(t)
	at := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	srv.SetOAuthStatus(fakeOAuthStatus{oauthusage.Status{
		Available:   true,
		LastAttempt: at,
		LastSuccess: at,
	}})

	_, body := getOAuthStatus(t, srv)
	if body["enabled"] != true || body["available"] != true {
		t.Errorf("enabled/available = %v/%v, want true/true", body["enabled"], body["available"])
	}
	if body["credential_stale"] != false {
		t.Errorf("credential_stale = %v, want false", body["credential_stale"])
	}
	want := at.Format(time.RFC3339)
	if body["last_attempt"] != want || body["last_success"] != want {
		t.Errorf("last_attempt/last_success = %v/%v, want %s", body["last_attempt"], body["last_success"], want)
	}
}

// The poller stamps local wall-clock times; the endpoint reports UTC so it
// matches /slack's timestamps.
func TestHandleOAuthStatus_TimesReportedInUTC(t *testing.T) {
	srv := newOAuthStatusServer(t)
	local := time.Date(2026, 9, 24, 9, 9, 34, 817224100, time.FixedZone("CDT", -5*3600))
	srv.SetOAuthStatus(fakeOAuthStatus{oauthusage.Status{
		Available:   true,
		LastAttempt: local,
		LastSuccess: local,
	}})

	_, body := getOAuthStatus(t, srv)
	want := "2026-09-24T14:09:34.8172241Z"
	if body["last_attempt"] != want || body["last_success"] != want {
		t.Errorf("last_attempt/last_success = %v/%v, want %s", body["last_attempt"], body["last_success"], want)
	}
}

// A failure after an earlier success keeps last_success, so a caller can
// tell how long the source has been down.
func TestHandleOAuthStatus_StaleCredentialAfterSuccess(t *testing.T) {
	srv := newOAuthStatusServer(t)
	ok := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	failed := ok.Add(3 * time.Hour)
	srv.SetOAuthStatus(fakeOAuthStatus{oauthusage.Status{
		Available:       false,
		CredentialStale: true,
		LastAttempt:     failed,
		LastSuccess:     ok,
		LastError:       "credential stale: access token expired",
	}})

	_, body := getOAuthStatus(t, srv)
	if body["available"] != false || body["credential_stale"] != true {
		t.Errorf("available/credential_stale = %v/%v, want false/true", body["available"], body["credential_stale"])
	}
	if body["last_attempt"] != failed.Format(time.RFC3339) {
		t.Errorf("last_attempt = %v, want %s", body["last_attempt"], failed.Format(time.RFC3339))
	}
	if body["last_success"] != ok.Format(time.RFC3339) {
		t.Errorf("last_success = %v, want %s", body["last_success"], ok.Format(time.RFC3339))
	}
	if body["last_error"] != "credential stale: access token expired" {
		t.Errorf("last_error = %v", body["last_error"])
	}
}

// A refresh that did not help leaves the source disarmed until it
// recovers; the endpoint is how anyone outside the process can see that.
func TestHandleOAuthStatus_RefreshState(t *testing.T) {
	srv := newOAuthStatusServer(t)
	at := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	srv.SetOAuthStatus(fakeOAuthStatus{oauthusage.Status{
		CredentialStale:    true,
		LastAttempt:        at,
		RefreshEnabled:     true,
		RefreshArmed:       false,
		LastRefreshAttempt: at,
		LastRefreshError:   "credential still stale after refresh",
	}})

	_, body := getOAuthStatus(t, srv)
	if body["refresh_enabled"] != true || body["refresh_armed"] != false {
		t.Errorf("refresh_enabled/refresh_armed = %v/%v, want true/false", body["refresh_enabled"], body["refresh_armed"])
	}
	if body["last_refresh_attempt"] != at.Format(time.RFC3339) {
		t.Errorf("last_refresh_attempt = %v, want %s", body["last_refresh_attempt"], at.Format(time.RFC3339))
	}
	if body["last_refresh_error"] != "credential still stale after refresh" {
		t.Errorf("last_refresh_error = %v", body["last_refresh_error"])
	}
}

// Before any refresh the attempt time is null, like the other times.
func TestHandleOAuthStatus_NoRefreshYet(t *testing.T) {
	srv := newOAuthStatusServer(t)
	srv.SetOAuthStatus(fakeOAuthStatus{oauthusage.Status{RefreshEnabled: true, RefreshArmed: true}})

	_, body := getOAuthStatus(t, srv)
	if body["refresh_enabled"] != true || body["refresh_armed"] != true {
		t.Errorf("refresh_enabled/refresh_armed = %v/%v, want true/true", body["refresh_enabled"], body["refresh_armed"])
	}
	if v, ok := body["last_refresh_attempt"]; !ok || v != nil {
		t.Errorf("last_refresh_attempt = %v (present=%v), want null", v, ok)
	}
}

// The real poller satisfies the interface the server accepts, so the
// trayapp wiring compiles against the concrete type.
func TestPollerSatisfiesOAuthStatusSource(t *testing.T) {
	var _ OAuthStatusSource = (*oauthusage.Poller)(nil)
}
