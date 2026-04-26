//go:build !windows

package main

import (
	"context"
	"log/slog"

	"github.com/vector76/cc_usage_dashboard/internal/server"
)

// StartTray is the non-Windows no-op implementation of the tray UI. The
// trayapp still runs as a headless HTTP server on Linux/macOS; this stub
// satisfies the build-tag pattern documented in docs/development.md and
// blocks until the supplied context is cancelled so the calling goroutine
// has a stable lifetime regardless of platform.
func StartTray(ctx context.Context, srv *server.Server, paused interface{ Toggle() }, dashboardURL string) {
	slog.Info("tray UI not available on this platform; running headless", "dashboard", dashboardURL)
	<-ctx.Done()
}
