package oauthusage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestClient points a Client at a stub server.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient()
	c.BaseURL = srv.URL
	return c
}

func TestClientFetchSuccess(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "usage_response.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != usagePath {
			t.Errorf("path = %q, want %q", r.URL.Path, usagePath)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Write(body)
	})

	got, err := c.Fetch(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	wantFloat(t, "SessionUsed", got.SessionUsed, 3)
	wantFloat(t, "WeeklyUsed", got.WeeklyUsed, 29)
}

// TestClientSendsOnlyAuthorization pins an empirical finding: five header
// variants sent back to back with the same token — including no
// User-Agent at all and no anthropic-beta — all returned identical
// payloads. Only Authorization is required.
//
// Sending a spoofed claude-code User-Agent would also misrepresent this
// client as Claude Code itself, so the test pins its absence rather than
// leaving it as a stylistic preference.
func TestClientSendsOnlyAuthorization(t *testing.T) {
	body, _ := os.ReadFile(filepath.Join("testdata", "usage_response.json"))

	var gotAuth, gotBeta, gotUA string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotUA = r.Header.Get("User-Agent")
		w.Write(body)
	})

	if _, err := c.Fetch(context.Background(), "tok"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if gotBeta != "" {
		t.Errorf("anthropic-beta = %q, want it unset", gotBeta)
	}
	if gotUA == "" {
		t.Error("User-Agent should identify this client, not be blank")
	}
	if strings.HasPrefix(gotUA, "claude-code") {
		t.Errorf("User-Agent = %q, must not impersonate Claude Code", gotUA)
	}
}

func TestClientFetchClassifiesErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		fixture   string
		wantStale bool
	}{
		{name: "expired", status: 401, fixture: "error_expired.json", wantStale: true},
		{name: "invalid", status: 401, fixture: "error_invalid.json", wantStale: true},
		{name: "server error", status: 500, fixture: "", wantStale: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.fixture != "" {
				body, _ = os.ReadFile(filepath.Join("testdata", tc.fixture))
			}
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write(body)
			})

			_, err := c.Fetch(context.Background(), "tok")
			if err == nil {
				t.Fatal("Fetch: got nil error")
			}
			if got := errors.Is(err, ErrCredentialStale); got != tc.wantStale {
				t.Errorf("errors.Is(err, ErrCredentialStale) = %v, want %v (err: %v)",
					got, tc.wantStale, err)
			}
		})
	}
}

// TestClientFetchHonorsContext keeps a hung endpoint from pinning a poll
// tick open indefinitely, which at shutdown would hold up process exit.
func TestClientFetchHonorsContext(t *testing.T) {
	released := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-released
	})
	t.Cleanup(func() { close(released) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Fetch(ctx, "tok"); err == nil {
		t.Fatal("expected a timeout error")
	}
}

// TestClientRejectsOversizedBody keeps a misrouted response (a proxy error
// page, say) from being read into memory without bound.
func TestClientRejectsOversizedBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		junk := make([]byte, maxResponseBytes+1024)
		for i := range junk {
			junk[i] = 'x'
		}
		w.Write(junk)
	})

	if _, err := c.Fetch(context.Background(), "tok"); err == nil {
		t.Fatal("expected an error for an oversized body")
	}
}
