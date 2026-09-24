package oauthusage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withRefresh attaches a refresh hook to the harness's poller and returns a
// counter of how many times it ran.
func withRefresh(h *pollerHarness, fn func(ctx context.Context) error) *int64 {
	var calls int64
	h.poller.refresh = func(ctx context.Context) error {
		atomic.AddInt64(&calls, 1)
		return fn(ctx)
	}
	return &calls
}

// A refresh that rotates the token recovers the source within the same
// tick: the poll is retried at once rather than a whole interval later.
func TestRefreshOnStaleRetriesPollImmediately(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))
	calls := withRefresh(h, func(context.Context) error {
		h.writeToken(t, "tok-fresh", now.Add(8*time.Hour))
		return nil
	})

	if err := h.poller.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("refresh ran %d times, want 1", n)
	}
	if got := h.snapshots(); len(got) != 1 {
		t.Fatalf("recorded %d snapshots, want 1", len(got))
	}
	if seen := h.tokens.all(); len(seen) != 1 || seen[0] != "Bearer tok-fresh" {
		t.Errorf("requests = %v, want one with the refreshed token", seen)
	}
	st := h.poller.Status()
	if !st.Available || st.CredentialStale {
		t.Errorf("available/stale = %v/%v, want true/false", st.Available, st.CredentialStale)
	}
	if !st.RefreshArmed {
		t.Error("a successful poll must re-arm the refresh")
	}
	if !st.LastRefreshAttempt.Equal(now) {
		t.Errorf("LastRefreshAttempt = %s, want %s", st.LastRefreshAttempt, now)
	}
	if st.LastRefreshError != "" {
		t.Errorf("LastRefreshError = %q, want empty", st.LastRefreshError)
	}
}

// The guard against hammering: a refresh that leaves the token stale is not
// tried again, however many polls follow, until the credential recovers.
func TestRefreshThatDoesNotHelpIsNotRetried(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))
	calls := withRefresh(h, func(context.Context) error { return nil })

	for i := 0; i < 5; i++ {
		err := h.poller.poll(context.Background())
		if !errors.Is(err, ErrCredentialStale) {
			t.Fatalf("poll %d: got %v, want ErrCredentialStale", i, err)
		}
	}

	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("refresh ran %d times over 5 stale polls, want 1", n)
	}
	st := h.poller.Status()
	if st.RefreshArmed {
		t.Error("refresh must stay disarmed while the credential is still stale")
	}
	if !strings.Contains(st.LastRefreshError, "still stale") {
		t.Errorf("LastRefreshError = %q, want it to say the credential is still stale", st.LastRefreshError)
	}
}

// A command that fails outright disarms just the same.
func TestRefreshErrorDisarms(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))
	calls := withRefresh(h, func(context.Context) error { return fmt.Errorf("exit status 1") })

	_ = h.poller.poll(context.Background())
	_ = h.poller.poll(context.Background())

	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("refresh ran %d times, want 1", n)
	}
	st := h.poller.Status()
	if st.RefreshArmed {
		t.Error("a failed refresh must disarm")
	}
	if !strings.Contains(st.LastRefreshError, "exit status 1") {
		t.Errorf("LastRefreshError = %q, want the command's error", st.LastRefreshError)
	}
}

// Once the credential is good again -- here because Claude Code ran on its
// own -- the next stale episode gets its own single attempt.
func TestRefreshRearmsAfterRecovery(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	clock := now
	h := newPollerHarness(t, func() time.Time { return clock })
	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))
	calls := withRefresh(h, func(context.Context) error { return nil })

	_ = h.poller.poll(context.Background())
	_ = h.poller.poll(context.Background())
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Fatalf("first episode: refresh ran %d times, want 1", n)
	}

	// Claude Code runs and rotates the token.
	h.writeToken(t, "tok-fresh", clock.Add(8*time.Hour))
	if err := h.poller.poll(context.Background()); err != nil {
		t.Fatalf("poll after recovery: %v", err)
	}
	if !h.poller.Status().RefreshArmed {
		t.Fatal("recovery must re-arm the refresh")
	}

	// It expires again.
	clock = clock.Add(9 * time.Hour)
	_ = h.poller.poll(context.Background())
	_ = h.poller.poll(context.Background())
	if n := atomic.LoadInt64(calls); n != 2 {
		t.Errorf("after the second episode refresh ran %d times total, want 2", n)
	}
}

// Only a stale credential calls for a refresh. A broken endpoint would not
// be fixed by running Claude Code.
func TestRefreshNotRunForOtherFailures(t *testing.T) {
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

	var calls int64
	p := NewPoller(PollerConfig{
		Interval:        time.Minute,
		CredentialsPath: credPath,
		Client:          client,
		Now:             func() time.Time { return now },
		Sink:            func(Snapshot) error { return nil },
		Refresh: func(context.Context) error {
			atomic.AddInt64(&calls, 1)
			return nil
		},
	})

	err := p.poll(context.Background())
	if err == nil || errors.Is(err, ErrCredentialStale) {
		t.Fatalf("got %v, want a non-stale error", err)
	}
	if n := atomic.LoadInt64(&calls); n != 0 {
		t.Errorf("refresh ran %d times, want 0", n)
	}
	st := p.Status()
	if !st.RefreshEnabled || !st.RefreshArmed {
		t.Errorf("enabled/armed = %v/%v, want true/true: an unrelated failure must not spend the attempt",
			st.RefreshEnabled, st.RefreshArmed)
	}
}

// A missing credentials file counts as stale (see LoadCredential), and
// running Claude Code is what would recreate it, so it gets the one try.
func TestRefreshRunsForMissingCredentialsFile(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	calls := withRefresh(h, func(context.Context) error {
		h.writeToken(t, "tok-new", now.Add(8*time.Hour))
		return nil
	})

	if err := h.poller.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("refresh ran %d times, want 1", n)
	}
}

// Without a hook the poller behaves exactly as before.
func TestNoRefreshConfigured(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))

	err := h.poller.poll(context.Background())
	if !errors.Is(err, ErrCredentialStale) {
		t.Fatalf("got %v, want ErrCredentialStale", err)
	}
	st := h.poller.Status()
	if st.RefreshEnabled {
		t.Error("RefreshEnabled must be false without a hook")
	}
	if !st.LastRefreshAttempt.IsZero() {
		t.Error("no refresh should have been attempted")
	}
}

// A hung command must not stall the poll loop.
func TestRefreshIsBoundedByTimeout(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := newPollerHarness(t, func() time.Time { return now })
	h.writeToken(t, "tok-expired", now.Add(-1*time.Hour))
	h.poller.refreshTimeout = 50 * time.Millisecond
	withRefresh(h, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	done := make(chan struct{})
	go func() {
		_ = h.poller.poll(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not return after the refresh timeout")
	}
	if h.poller.Status().RefreshArmed {
		t.Error("a timed-out refresh must disarm")
	}
}

// TestHelperProcess is not a real test: the command refresher tests run the
// test binary itself as the child, and this is its body.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("OAUTHUSAGE_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	wd, _ := os.Getwd()
	_ = os.WriteFile(filepath.Join(wd, "helper_ran.txt"), []byte(strings.Join(args, " ")), 0600)
	if os.Getenv("OAUTHUSAGE_HELPER_FAIL") == "1" {
		fmt.Fprint(os.Stderr, "not logged in")
		os.Exit(3)
	}
	os.Exit(0)
}

func helperRefresher(t *testing.T, dir string, fail bool) RefreshFunc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv("OAUTHUSAGE_HELPER", "1")
	if fail {
		t.Setenv("OAUTHUSAGE_HELPER_FAIL", "1")
	}
	return newCommandRefresher(exe, []string{"-test.run=^TestHelperProcess$", "--", "-p", "/usage"}, dir)
}

func TestCommandRefresherRunsInDirWithArgs(t *testing.T) {
	dir := t.TempDir()
	if err := helperRefresher(t, dir, false)(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "helper_ran.txt"))
	if err != nil {
		t.Fatalf("child did not run in %s: %v", dir, err)
	}
	if string(got) != "-p /usage" {
		t.Errorf("child args = %q, want %q", got, "-p /usage")
	}
}

// A failing command's output is what explains it, so it rides in the error.
func TestCommandRefresherReportsFailureOutput(t *testing.T) {
	err := helperRefresher(t, t.TempDir(), true)(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("error %q should carry the command's output", err)
	}
}

func TestClaudeRefreshArgs(t *testing.T) {
	if got := strings.Join(ClaudeRefreshArgs, " "); got != "-p /usage" {
		t.Errorf("ClaudeRefreshArgs = %q, want %q", got, "-p /usage")
	}
}
