package server

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// authWarnInterval throttles the rejected-request warning. A client holding
// a stale token retries on every tick (the uplink) or every transcript line
// (the Stop hook), and one warning per request would bury everything else in
// the feedback buffer. The auth_rejected_total counter still counts them all.
const authWarnInterval = time.Minute

// TokenChecker validates the bearer token a non-loopback caller presents.
// authtoken.Store implements it; the interface keeps the server free of the
// token's storage and rotation.
type TokenChecker interface {
	Check(presented string) bool
}

// SetAuth installs the token gate: from then on every request whose peer is
// not a loopback address must carry "Authorization: Bearer <token>". nil (the
// default, used by unit tests) disables the gate. Safe to call once before
// serving traffic; the checker itself may change its token concurrently.
func (s *Server) SetAuth(c TokenChecker) {
	s.auth = c
}

// authorized reports whether r may proceed past the token gate.
//
// Loopback peers are exempt: anything that can open a loopback connection is
// already running on this machine, which is the trust boundary the app has
// always had, and it keeps the userscript and dashboard working with no
// setup. Every other peer — VMs, LAN hosts, and Docker/WSL adapters alike —
// must present the token.
func (s *Server) authorized(r *http.Request) bool {
	if s.auth == nil || isLoopbackPeer(r.RemoteAddr) {
		return true
	}
	tok, ok := bearerToken(r.Header.Get("Authorization"))
	return ok && s.auth.Check(tok)
}

// rejectUnauthorized answers 401 and records the rejection. The presented
// token is never logged.
func (s *Server) rejectUnauthorized(w http.ResponseWriter, r *http.Request) {
	s.metrics.AuthRejected.Add(1)

	now := time.Now().UnixNano()
	last := s.lastAuthWarn.Load()
	if now-last >= int64(authWarnInterval) && s.lastAuthWarn.CompareAndSwap(last, now) {
		slog.Warn("rejected request without a valid access token (further rejections suppressed for a minute; see auth_rejected_total)",
			"remote", r.RemoteAddr, "path", r.URL.Path,
			"has_authorization", r.Header.Get("Authorization") != "")
	}

	w.Header().Set("WWW-Authenticate", `Bearer realm="usage_dashboard"`)
	writeJSONError(w, http.StatusUnauthorized, "missing or invalid access token")
}

// isLoopbackPeer reports whether a RemoteAddr ("ip:port") is a loopback
// address. Anything unparseable is treated as remote.
func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerToken extracts the token from an Authorization header value. The
// scheme is case-insensitive per RFC 7235.
func bearerToken(header string) (string, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(rest)
	return tok, tok != ""
}
