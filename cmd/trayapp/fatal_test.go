package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Before logging exists (a config.yaml typo, say), a windowsgui build has
// nowhere to write: stderr is discarded. The failure must land in the
// well-known data-dir log so the operator can find out why no icon appeared.
func TestReportStartupFailureWritesFallbackFileWithoutConsole(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "sub", "trayapp.log")
	var stderr bytes.Buffer

	reportStartupFailure(&stderr, "failed to load config: bad yaml", false, false, fallback)

	got, err := os.ReadFile(fallback)
	if err != nil {
		t.Fatalf("fallback log not written: %v", err)
	}
	if !strings.Contains(string(got), "failed to load config: bad yaml") {
		t.Errorf("fallback log = %q, want the failure message", got)
	}
	if !strings.Contains(stderr.String(), "failed to load config: bad yaml") {
		t.Errorf("stderr = %q, want the failure message as before", stderr.String())
	}
}

// Appending, not truncating: the fallback is the same trayapp.log earlier
// runs rotated into, and its history is what the operator is reading.
func TestReportStartupFailureAppendsToExistingFallback(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "trayapp.log")
	if err := os.WriteFile(fallback, []byte("earlier line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reportStartupFailure(&bytes.Buffer{}, "boom", false, false, fallback)

	got, _ := os.ReadFile(fallback)
	if !strings.HasPrefix(string(got), "earlier line\n") || !strings.Contains(string(got), "boom") {
		t.Errorf("fallback log = %q, want the earlier line kept and the failure appended", got)
	}
}

// With a console attached, stderr is a real destination; no stray file.
func TestReportStartupFailureSkipsFallbackWithConsole(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "trayapp.log")
	var stderr bytes.Buffer

	reportStartupFailure(&stderr, "boom", false, true, fallback)

	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Errorf("fallback log created with a console attached (stat err %v)", err)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr = %q, want the failure message", stderr.String())
	}
}

// Once the rotating log is open (an unopenable database, the case logdest.go
// exists for), the failure goes through slog so it lands in that log.
func TestReportStartupFailureUsesSlogOnceLogFileIsOpen(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fallback := filepath.Join(t.TempDir(), "trayapp.log")

	reportStartupFailure(&bytes.Buffer{}, "failed to open database x: locked", true, false, fallback)

	if !strings.Contains(logged.String(), "failed to open database x: locked") ||
		!strings.Contains(logged.String(), `"level":"ERROR"`) {
		t.Errorf("slog output = %q, want an ERROR record with the message", logged.String())
	}
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Errorf("fallback log written although the rotating log is open (stat err %v)", err)
	}
}
