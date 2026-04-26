package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anthropics/usage-dashboard/internal/config"
	"github.com/anthropics/usage-dashboard/internal/store"
)

// TestShutdownDrainsInFlightRequest verifies that Shutdown waits for an
// in-flight handler to return its response before unblocking, and that
// the underlying ListenAndServe goroutine exits with http.ErrServerClosed.
func TestShutdownDrainsInFlightRequest(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// slowHandler blocks on `released` so the test can hold a request
	// open while Shutdown is in progress. We register it directly on
	// srv.mux because shutdown_test.go lives in the same package.
	released := make(chan struct{})
	handlerStarted := make(chan struct{})
	srv.mux.HandleFunc("GET /test_slow", func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-released
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "done")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(ln)
	}()

	// Issue the slow request from a separate goroutine so the test can
	// drive Shutdown while the handler is mid-flight.
	respCh := make(chan *http.Response, 1)
	reqErrCh := make(chan error, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://%s/test_slow", addr))
		if err != nil {
			reqErrCh <- err
			return
		}
		respCh <- resp
	}()

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	// Kick off Shutdown. It must not return until the in-flight request
	// finishes, which only happens once we close `released`.
	shutdownDone := make(chan error, 1)
	shutdownStart := time.Now()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownDone <- srv.Shutdown(ctx)
	}()

	// Confirm Shutdown is genuinely blocked on the in-flight request.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before in-flight request finished: err=%v", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(released)

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return within 10s")
	}

	if elapsed := time.Since(shutdownStart); elapsed > 10*time.Second {
		t.Fatalf("Shutdown took longer than 10s: %v", elapsed)
	}

	select {
	case resp := <-respCh:
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	case err := <-reqErrCh:
		t.Fatalf("in-flight request failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not complete")
	}

	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve goroutine did not exit after Shutdown")
	}
}

// TestCheckpointDoesNotGrowDB verifies the WAL sidecar does not grow
// after Checkpoint() is invoked following a batch of writes. A passing
// shutdown leaves the on-disk state consolidated rather than relying on
// SQLite's automatic checkpoint cadence.
func TestCheckpointDoesNotGrowDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Write enough rows to populate the WAL beyond the auto-checkpoint
	// threshold (default 1000 pages). 2000 inserts comfortably exceeds
	// it on the typical 4KiB page size.
	for i := 0; i < 2000; i++ {
		cost := 0.001
		_, err := s.InsertUsageEvent(
			time.Now(),
			"api",
			fmt.Sprintf("session-%d", i),
			fmt.Sprintf("msg-%d", i),
			"/some/project",
			"claude-3-5-sonnet-20241022",
			100, 50, 0, 0,
			&cost,
			"reported",
			`{"raw":"json"}`,
		)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	walPath := dbPath + "-wal"
	walBefore := fileSize(t, walPath)

	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	walAfter := fileSize(t, walPath)

	if walAfter > walBefore {
		t.Fatalf("WAL grew after checkpoint: before=%d after=%d", walBefore, walAfter)
	}
}

// TestShutdownSequenceWithinDeadline exercises the full graceful path
// (drain HTTP, checkpoint, close) and asserts it completes well under
// the 10s budget the trayapp grants.
func TestShutdownSequenceWithinDeadline(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	cfg := &config.Config{}
	cfg.HTTP.Port = 0
	cfg.Database.Path = dbPath
	cfg.Pricing.TablePath = ""

	srv := New(s, cfg)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()

	// Give Serve a moment to start accepting.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("shutdown sequence exceeded 10s budget: %v", elapsed)
	}

	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
