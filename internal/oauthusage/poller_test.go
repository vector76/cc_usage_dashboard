package oauthusage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pollerHarness wires a Poller to a stub endpoint and a recording sink.
type pollerHarness struct {
	poller   *Poller
	credPath string
	requests *int64
	tokens   *tokenLog

	mu   sync.Mutex
	sent []Snapshot
}

type tokenLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *tokenLog) add(v string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, v)
}

func (l *tokenLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

func (h *pollerHarness) snapshots() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Snapshot(nil), h.sent...)
}

// writeToken rewrites the credentials file, as Claude Code does when it
// rotates the access token.
func (h *pollerHarness) writeToken(t *testing.T, token string, expires time.Time) {
	t.Helper()
	body := `{"claudeAiOauth":{"accessToken":"` + token +
		`","expiresAt":` + strconv.FormatInt(msSinceEpoch(expires), 10) + `}}`
	if err := os.WriteFile(h.credPath, []byte(body), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

func newPollerHarness(t *testing.T, now func() time.Time) *pollerHarness {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "usage_response.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var requests int64
	tokens := &tokenLog{}
	h := &pollerHarness{
		credPath: filepath.Join(t.TempDir(), ".credentials.json"),
		requests: &requests,
		tokens:   tokens,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		tokens.add(r.Header.Get("Authorization"))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	client := NewClient()
	client.BaseURL = srv.URL

	h.poller = NewPoller(PollerConfig{
		Interval:        50 * time.Millisecond,
		CredentialsPath: h.credPath,
		Client:          client,
		Now:             now,
		Sink: func(s Snapshot) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.sent = append(h.sent, s)
			return nil
		},
	})
	return h
}

func TestPollOnceRecordsSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-1", now.Add(8*time.Hour))

	if err := h.poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	got := h.snapshots()
	if len(got) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(got))
	}
	wantFloat(t, "SessionUsed", got[0].SessionUsed, 3)
	if !got[0].ObservedAt.Equal(now) {
		t.Errorf("ObservedAt = %s, want %s", got[0].ObservedAt, now)
	}
	// Cold start: nothing to chain off.
	if got[0].ContinuousWithPrev == nil || *got[0].ContinuousWithPrev {
		t.Errorf("first snapshot should report continuous_with_prev false")
	}
}

func TestPollOnceSecondReadIsContinuous(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	clock := now
	h := newPollerHarness(t, func() time.Time { return clock })
	h.writeToken(t, "tok-1", now.Add(8*time.Hour))

	if err := h.poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first pollOnce: %v", err)
	}
	clock = clock.Add(3 * time.Minute)
	if err := h.poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second pollOnce: %v", err)
	}

	got := h.snapshots()
	if len(got) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(got))
	}
	if got[1].ContinuousWithPrev == nil || !*got[1].ContinuousWithPrev {
		t.Error("second snapshot should report continuous_with_prev true")
	}
}

// TestPollOnceSkipsRequestWhenCredentialExpired is the whole point of the
// pre-flight check: a host idle overnight holds an expired token, and
// firing a request we know will 401 wastes a round trip and fills the log
// with a failure we could have predicted locally.
func TestPollOnceSkipsRequestWhenCredentialExpired(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))

	err := h.poller.PollOnce(context.Background())
	if !errors.Is(err, ErrCredentialStale) {
		t.Errorf("got %v, want ErrCredentialStale", err)
	}
	if n := atomic.LoadInt64(h.requests); n != 0 {
		t.Errorf("made %d HTTP requests, want 0", n)
	}
	if got := h.snapshots(); len(got) != 0 {
		t.Errorf("recorded %d snapshots, want 0", len(got))
	}
}

// TestPollOnceRereadsCredentialEachPoll pins the rule that the token is
// read fresh every time. Claude Code rewrites the file when it rotates,
// and a cached copy goes stale within the day.
func TestPollOnceRereadsCredentialEachPoll(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })

	h.writeToken(t, "tok-first", now.Add(8*time.Hour))
	if err := h.poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first pollOnce: %v", err)
	}
	h.writeToken(t, "tok-rotated", now.Add(8*time.Hour))
	if err := h.poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second pollOnce: %v", err)
	}

	seen := h.tokens.all()
	if len(seen) != 2 {
		t.Fatalf("saw %d requests, want 2", len(seen))
	}
	if seen[0] != "Bearer tok-first" {
		t.Errorf("first request used %q", seen[0])
	}
	if seen[1] != "Bearer tok-rotated" {
		t.Errorf("second request used %q, want the rotated token", seen[1])
	}
}

// TestStatusDistinguishesStaleFromBroken is the behavior that keeps the
// tray honest. A stale credential is a self-healing "temporarily
// unavailable", not a "the scraper broke, go fix it" — routing it to the
// latter would make the parse-error health signal untrustworthy.
func TestStatusDistinguishesStaleFromBroken(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })

	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))
	_ = h.poller.PollOnce(context.Background())

	st := h.poller.Status()
	if st.Available {
		t.Error("Available should be false while the credential is stale")
	}
	if !st.CredentialStale {
		t.Error("CredentialStale should be true")
	}
	if st.LastError == "" {
		t.Error("LastError should describe the condition")
	}

	// Claude Code runs and rotates the token: the source recovers on its
	// own, with no intervention and no restart.
	h.writeToken(t, "tok-fresh", now.Add(8*time.Hour))
	if err := h.poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce after refresh: %v", err)
	}

	st = h.poller.Status()
	if !st.Available {
		t.Error("Available should be true after a successful poll")
	}
	if st.CredentialStale {
		t.Error("CredentialStale should clear after a successful poll")
	}
	if st.LastError != "" {
		t.Errorf("LastError should clear, got %q", st.LastError)
	}
	if !st.LastSuccess.Equal(now) {
		t.Errorf("LastSuccess = %s, want %s", st.LastSuccess, now)
	}
}

// TestPollOnceDoesNotRecordOnServerError keeps a bad response from landing
// in the database as a partial or empty reading.
func TestPollOnceDoesNotRecordOnServerError(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	credPath := filepath.Join(t.TempDir(), ".credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"tok","expiresAt":` +
		strconv.FormatInt(msSinceEpoch(now.Add(8*time.Hour)), 10) + `}}`
	if err := os.WriteFile(credPath, []byte(body), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := NewClient()
	client.BaseURL = srv.URL

	var recorded int
	p := NewPoller(PollerConfig{
		Interval:        time.Minute,
		CredentialsPath: credPath,
		Client:          client,
		Now:             func() time.Time { return now },
		Sink:            func(Snapshot) error { recorded++; return nil },
	})

	err := p.PollOnce(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrCredentialStale) {
		t.Error("a 500 must not be classified as a credential problem")
	}
	if recorded != 0 {
		t.Errorf("recorded %d snapshots, want 0", recorded)
	}
	if st := p.Status(); st.CredentialStale {
		t.Error("CredentialStale must not be set by a server error")
	}
}

// TestStartPollsImmediately keeps the first reading from being withheld
// for a full interval after launch — three minutes of blank chart at every
// start would be a poor trade for nothing.
func TestStartPollsImmediately(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-1", now.Add(8*time.Hour))

	h.poller.Start()
	t.Cleanup(h.poller.Stop)

	deadline := time.After(2 * time.Second)
	for {
		if len(h.snapshots()) > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("no snapshot recorded within 2s of Start")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestStopIsPromptAndIdempotent matters at shutdown: the trayapp bounds
// background goroutines with a 10s timeout, and a poller that ignored Stop
// would burn that budget on every exit.
func TestStopIsPromptAndIdempotent(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-1", now.Add(8*time.Hour))

	h.poller.Start()

	done := make(chan struct{})
	go func() {
		h.poller.Stop()
		h.poller.Stop() // a second call must not panic on a closed channel
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}
}
