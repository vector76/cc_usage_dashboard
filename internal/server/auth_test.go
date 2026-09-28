package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeToken is a TokenChecker whose token a test can swap, standing in for a
// tray-menu rotation.
type fakeToken struct {
	mu  sync.Mutex
	tok string
}

func (f *fakeToken) Check(presented string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tok != "" && presented == f.tok
}

func (f *fakeToken) set(tok string) {
	f.mu.Lock()
	f.tok = tok
	f.mu.Unlock()
}

func authServer(t *testing.T, tok string) (*Server, *fakeToken) {
	t.Helper()
	srv, st := createTestServer(t)
	t.Cleanup(func() { st.Close() })
	checker := &fakeToken{tok: tok}
	srv.SetAuth(checker)
	return srv, checker
}

func getFrom(remote, authHeader string) *http.Request {
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = remote
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// Loopback callers — the userscript in the host browser, the dashboard,
// manual curl on the host — never need the token.
func TestAuthExemptsLoopback(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	for _, remote := range []string{"127.0.0.1:50000", "[::1]:50000", "[::ffff:127.0.0.1]:50000", "127.0.0.2:50000"} {
		t.Run(remote, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, getFrom(remote, ""))
			if w.Code != http.StatusOK {
				t.Errorf("loopback %s: expected 200 without a token, got %d", remote, w.Code)
			}
		})
	}
}

// Everything that is not loopback needs the token — including Docker and
// WSL adapters, which were trusted implicitly before the gate existed.
func TestAuthRequiresTokenFromNonLoopback(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	for _, remote := range []string{"192.168.56.10:50000", "172.17.0.2:50000", "10.0.0.5:50000", "[fe80::1]:50000"} {
		t.Run(remote, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, getFrom(remote, ""))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", w.Code)
			}
			if got := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
				t.Errorf("401 must advertise the Bearer scheme, got %q", got)
			}
		})
	}
}

func TestAuthAcceptsCorrectToken(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	for _, header := range []string{"Bearer s3cret", "bearer s3cret", "Bearer  s3cret "} {
		t.Run(header, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, getFrom("192.168.56.10:50000", header))
			if w.Code != http.StatusOK {
				t.Errorf("expected 200 for %q, got %d", header, w.Code)
			}
		})
	}
}

func TestAuthRejectsWrongOrMalformedToken(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	for _, header := range []string{"Bearer wrong", "s3cret", "Basic s3cret", "Bearer", "Bearer "} {
		t.Run(header, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, getFrom("192.168.56.10:50000", header))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 for %q, got %d", header, w.Code)
			}
		})
	}
}

// An unparseable RemoteAddr cannot be proven loopback, so it is treated as
// remote.
func TestAuthTreatsUnparseableRemoteAsRemote(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, getFrom("garbage", ""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// The gate sits in front of the mux, so a rejected write never reaches the
// handler.
func TestAuthRejectedLogPostDoesNotInsert(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	srv.SetAuth(&fakeToken{tok: "s3cret"})

	req := jsonPOST("/log", []byte(`{"input_tokens":1,"output_tokens":1,"session_id":"s","message_id":"m"}`))
	req.RemoteAddr = "192.168.56.10:50000"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("unauthenticated /log inserted %d rows", n)
	}
}

// Rotation takes effect on the very next request, with no restart.
func TestAuthFollowsRotation(t *testing.T) {
	srv, checker := authServer(t, "old")
	checker.set("new")

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, getFrom("192.168.56.10:50000", "Bearer old"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("old token must stop working after rotation, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, getFrom("192.168.56.10:50000", "Bearer new"))
	if w.Code != http.StatusOK {
		t.Errorf("new token must work after rotation, got %d", w.Code)
	}
}

func TestAuthRejectionsAreCounted(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	for i := 0; i < 3; i++ {
		srv.ServeHTTP(httptest.NewRecorder(), getFrom("192.168.56.10:50000", "Bearer wrong"))
	}

	// httptest's default RemoteAddr (192.0.2.1) is not loopback.
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if !bytes.Contains(w.Body.Bytes(), []byte("auth_rejected_total 3")) {
		t.Errorf("expected auth_rejected_total 3 in /metrics, got:\n%s", w.Body.String())
	}
}

// Without SetAuth (the unit-test default) no token is demanded, mirroring
// how a nil Host allow-list disables that check.
func TestNoAuthConfiguredAllowsAll(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, getFrom("192.168.56.10:50000", ""))
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with no auth configured, got %d", w.Code)
	}
}

// A token-bearing caller may name the host however it likes — by machine
// name, mDNS name, or an address the allow-list never heard of. The Host
// check exists for DNS rebinding, and a rebound page cannot present a
// token it does not know.
func TestAuthenticatedRemoteSkipsHostCheck(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	srv.SetAllowedHosts([]string{"127.0.0.1"}, 27812)

	req := getFrom("192.168.56.10:50000", "Bearer s3cret")
	req.Host = "jamie-pc:27812"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for an authenticated caller using a hostname, got %d", w.Code)
	}
}

// Without a token, a remote caller is refused whatever Host it sends.
func TestUnauthenticatedRemoteRefusedWhateverHost(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	srv.SetAllowedHosts([]string{"127.0.0.1"}, 27812)

	for _, host := range []string{"127.0.0.1:27812", "jamie-pc:27812", "attacker.example"} {
		req := getFrom("192.168.56.10:50000", "")
		req.Host = host
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("Host=%q: unauthenticated remote request was accepted", host)
		}
	}
}

// Loopback is where the Host check still does its job: the token gate
// exempts loopback, so a page rebound to 127.0.0.1 is stopped only here.
func TestLoopbackStillEnforcesHostCheck(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	srv.SetAllowedHosts([]string{"127.0.0.1"}, 27812)

	req := getFrom("127.0.0.1:50000", "")
	req.Host = "attacker.example"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a rebound loopback request, got %d", w.Code)
	}

	// Presenting the token does not buy a loopback caller out of the
	// check: the rebinding defence must not hinge on a header.
	req = getFrom("127.0.0.1:50000", "Bearer s3cret")
	req.Host = "attacker.example"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a loopback request with a foreign Host even with a token, got %d", w.Code)
	}
}
