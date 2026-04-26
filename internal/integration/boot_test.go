// Package integration exercises the trayapp's background loops together to
// verify they start, expose healthz correctly, and shut down without leaking
// goroutines.
package integration

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/usage-dashboard/internal/config"
	"github.com/anthropics/usage-dashboard/internal/ingest"
	"github.com/anthropics/usage-dashboard/internal/server"
	"github.com/anthropics/usage-dashboard/internal/store"
)

// TestBootLoopsStartAndStopCleanly mirrors what cmd/trayapp/main does for the
// non-HTTP background loops: it starts the tailer, retention pruner, and
// windows ticker, asserts /healthz reports the tailer field, then drives
// shutdown and confirms no goroutines are leaked.
func TestBootLoopsStartAndStopCleanly(t *testing.T) {
	projectsDir := t.TempDir()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{}
	cfg.Database.Path = ":memory:"
	cfg.Claude.ProjectsDir = projectsDir
	cfg.Pricing.TablePath = ""
	cfg.Retention.ParseErrorsDays = 30
	cfg.Retention.SlackSamplesDays = 90

	srv := server.New(st, cfg)

	// Snapshot goroutine count *after* server creation but before launching
	// the loops we're responsible for, so we measure only what we add.
	baseline := runtime.NumGoroutine()

	tailer := ingest.NewTailer(projectsDir, st, nil)
	tailer.Start()
	srv.SetTailer(tailer)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Use short tick intervals so the loops actually fire at least once
	// during the test instead of just blocking on the ticker channel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = st.PruneParseErrors(30 * 24 * time.Hour)
				_ = st.PruneSlackSamples(90 * 24 * time.Hour)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		we := srv.WindowsEngine()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := we.UpdateWindows(); err != nil {
					slog.Error("update windows", "err", err)
				}
			}
		}
	}()

	// Give loops a chance to schedule.
	time.Sleep(80 * time.Millisecond)

	// Hit /healthz and assert the tailer_caught_up field is present.
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if _, ok := resp["tailer_caught_up"]; !ok {
		t.Errorf("/healthz response missing tailer_caught_up field; got %v", resp)
	}
	if status, _ := resp["status"].(string); status != "healthy" {
		t.Errorf("expected status healthy, got %v", resp["status"])
	}

	// Shut down the loops.
	close(stop)
	tailer.Stop()
	wg.Wait()

	// Allow the runtime a brief settling period for goroutine teardown
	// (fsnotify's internal helper, finalizers, etc.) before sampling.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 0 {
		t.Errorf("goroutine leak after shutdown: baseline=%d now=%d (leaked=%d)",
			baseline, runtime.NumGoroutine(), leaked)
	}
}
