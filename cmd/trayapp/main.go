package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	ccusage "github.com/vector76/cc_usage_dashboard"
	"github.com/vector76/cc_usage_dashboard/internal/config"
	"github.com/vector76/cc_usage_dashboard/internal/feedback"
	"github.com/vector76/cc_usage_dashboard/internal/ingest"
	"github.com/vector76/cc_usage_dashboard/internal/netbind"
	"github.com/vector76/cc_usage_dashboard/internal/server"
	"github.com/vector76/cc_usage_dashboard/internal/slack"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// pauseToggle adapts a *slack.Calculator into the tiny `interface{ Toggle() }`
// the tray UI consumes, so the Pause menu item flips the same in-memory
// pause flag the HTTP handlers read.
type pauseToggle struct{ c *slack.Calculator }

func (p pauseToggle) Toggle() { p.c.SetPaused(!p.c.IsPaused()) }

// tailerGroup collects every running Tailer (the primary ~/.claude/projects
// root plus, when resolved, the Cowork sessions root) so both /healthz
// reporting and shutdown treat them uniformly instead of the shutdown path
// only knowing about the first one.
type tailerGroup []*ingest.Tailer

// CaughtUp reports /healthz status: caught up only when every tailer is.
func (g tailerGroup) CaughtUp() bool {
	for _, t := range g {
		if !t.CaughtUp() {
			return false
		}
	}
	return true
}

// Stop stops every tailer in the group, waiting for each in turn.
func (g tailerGroup) Stop() {
	for _, t := range g {
		t.Stop()
	}
}

// Reimport triggers a full recovery re-walk on every tailer in the group.
// Runs synchronously (each Tailer.Reimport call blocks until its own
// re-walk finishes) — the HTTP handler wrapping this is expected to call
// it in its own goroutine so a slow, deliberately-inefficient recovery
// pass doesn't block the request.
func (g tailerGroup) Reimport() {
	for _, t := range g {
		t.Reimport()
	}
}

const Version = "0.0.1"

const (
	logRotateMaxSize    int64 = 10 * 1024 * 1024
	logRotateMaxBackups       = 5
	retentionInterval         = 5 * time.Minute
	windowsTickInterval       = 30 * time.Second
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.String("version", Version, "show version")
	flag.Parse()

	if *configPath == "" {
		*configPath = config.ResolveConfigPath()
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	if cfg.Database.Path == "" {
		cfg.Database.Path = config.ResolveDBPath()
	}
	if cfg.Claude.ProjectsDir == "" {
		cfg.Claude.ProjectsDir = config.ResolveProjectsDir()
	}

	dbDir := filepath.Dir(cfg.Database.Path)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create database directory: %v\n", err)
		os.Exit(1)
	}

	// Configure logging destination before opening the DB so startup errors
	// land in the rotated log when one is configured.
	logCloser := setupLogging(cfg.Logging.File, cfg.Logging.Level)
	if logCloser != nil {
		defer logCloser.Close()
	}

	db, err := store.Open(cfg.Database.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	// db.Close is invoked explicitly during graceful shutdown after the
	// WAL checkpoint, so we don't defer it here.

	slog.Info("Claude Usage Dashboard starting", "version", Version, "db", cfg.Database.Path)

	srv := server.New(db, cfg)

	// Resolve the price table once (explicit config path -> local prices.yaml
	// override -> embedded default) and share it between the server's cost
	// handlers and the tailer, so a single table drives all cost computation.
	priceTable, priceSource, err := ingest.ResolvePriceTable(
		cfg.Pricing.TablePath, config.PriceTableSearchDirs(), ccusage.DefaultPriceTableYAML)
	if err != nil {
		slog.Warn("price table load failed; cost computation disabled", "err", err, "source", priceSource)
	} else {
		slog.Info("price table resolved", "source", priceSource, "models", len(priceTable))
	}
	srv.SetPriceTable(priceTable)

	// Repair rows the then-loaded price table could not price properly now that
	// the current table may know them: costs left NULL, and ceiling estimates
	// that real rates should supersede. Measured costs are untouched; a failure
	// is diagnostic, not fatal.
	if _, err := ingest.BackfillCosts(db, priceTable); err != nil {
		slog.Warn("cost backfill failed", "err", err)
	}

	tailer := ingest.NewTailer(cfg.Claude.ProjectsDir, db, priceTable)
	tailer.Start()
	tailers := tailerGroup{tailer}

	// Cowork ("local agent mode") sessions each get their own private
	// .claude home nested under CoworkSessionsDir rather than the user's
	// real ~/.claude, so the primary tailer above never sees them. A second
	// tailer rooted one level up recursively finds every session's nested
	// projects/ dir as it's created — same JSONL schema, no hook needed.
	if cfg.Claude.CoworkSessionsDir != "" {
		coworkTailer := ingest.NewTailer(cfg.Claude.CoworkSessionsDir, db, priceTable)
		coworkTailer.Start()
		tailers = append(tailers, coworkTailer)
	}
	srv.SetTailer(tailers)
	srv.SetReimporter(tailers)

	// stop signals every background loop (retention pruner, windows ticker)
	// to exit; wg lets shutdown wait for them. Each tailer has its own
	// stopChan + doneChan and is stopped via tailers.Stop() so we don't
	// double-track them on the WaitGroup.
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go runRetentionLoop(&wg, stop, db, cfg)

	wg.Add(1)
	go runWindowsLoop(&wg, stop, srv)

	// Tray UI: blocks on Windows until Quit is chosen, no-op stub elsewhere.
	// Cancelling trayCtx during shutdown unblocks the stub and asks the
	// systray runtime to tear down on Windows. trayDone closes when
	// StartTray returns so the main loop can shut down the rest of the
	// process if the user quits via the tray menu.
	trayCtx, cancelTray := context.WithCancel(context.Background())
	defer cancelTray()
	dashboardURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTP.Port)
	trayDone := make(chan struct{})
	go func() {
		StartTray(trayCtx, srv, pauseToggle{c: srv.SlackCalculator()}, dashboardURL)
		close(trayDone)
	}()

	// Resolve bind addresses (loopback + detected Docker/WSL adapters + overrides).
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Warn("failed to enumerate network interfaces", "err", err)
		ifaces = nil
	}
	bindAddrs, err := netbind.SelectBindAddrs(ifaces, netbind.BindConfig{
		UserOverrides: cfg.HTTP.Bind,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to select bind addresses: %v\n", err)
		os.Exit(1)
	}

	// Configure the Host header allow-list before any goroutine starts
	// accepting traffic, so DNS-rebinding requests cannot slip in during
	// startup. The list combines every interface we bind to with the
	// well-known names (localhost, 127.0.0.1, host.docker.internal) that
	// the userscript and containers actually use.
	srv.SetAllowedHosts(bindAddrs, cfg.HTTP.Port)

	serverErr := make(chan error, len(bindAddrs))

	for _, host := range bindAddrs {
		addr := fmt.Sprintf("%s:%d", host, cfg.HTTP.Port)
		// srv.ListenAndServe logs "starting HTTP server" itself; don't log
		// again here or every address shows up twice in the log.
		go func(a string) {
			serverErr <- srv.ListenAndServe(a)
		}(addr)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	remaining := len(bindAddrs)
waitLoop:
	for {
		select {
		case <-sigChan:
			slog.Info("received shutdown signal")
			break waitLoop
		case <-trayDone:
			slog.Info("tray exited; shutting down")
			break waitLoop
		case err := <-serverErr:
			remaining--
			if err != nil {
				slog.Error("HTTP listener exited", "err", err, "remaining", remaining)
			}
			if remaining == 0 {
				slog.Error("all HTTP listeners exited")
				break waitLoop
			}
		}
	}

	slog.Info("shutting down gracefully")

	// Phase 1: drain in-flight HTTP requests with a 10s deadline so
	// long-running handlers (e.g. dashboard fetches) can complete.
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	if err := srv.Shutdown(httpCtx); err != nil {
		slog.Error("HTTP shutdown error", "err", err)
	}
	cancelHTTP()

	// Phase 2: stop background goroutines (retention pruner, windows
	// ticker, tailer, tray UI). tailers.Stop is invoked inside the
	// goroutine so a stuck tailer is also bounded by the 10s timeout
	// — otherwise a hung tailer blocks process exit indefinitely.
	close(stop)
	cancelTray()

	bgDone := make(chan struct{})
	go func() {
		tailers.Stop()
		wg.Wait()
		close(bgDone)
	}()
	select {
	case <-bgDone:
	case <-time.After(10 * time.Second):
		slog.Warn("background goroutines did not exit within 10s, continuing shutdown")
	}

	// Phase 3: consolidate the WAL into the main DB file so the on-disk
	// state is fully durable and the -wal sidecar is shrunk before we
	// close the connection.
	if err := db.Checkpoint(); err != nil {
		slog.Error("wal checkpoint failed", "err", err)
	}

	if err := db.Close(); err != nil {
		slog.Error("db close failed", "err", err)
	}

	slog.Info("shutdown complete")
}

// runRetentionLoop prunes parse_errors and slack_samples on a 5-minute cadence
// using the configured retention windows. Exits when stop is closed.
func runRetentionLoop(wg *sync.WaitGroup, stop <-chan struct{}, db *store.Store, cfg *config.Config) {
	defer wg.Done()
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			parseErrAge := time.Duration(cfg.Retention.ParseErrorsDays) * 24 * time.Hour
			if err := db.PruneParseErrors(parseErrAge); err != nil {
				slog.Error("prune parse errors", "err", err)
			}
			slackAge := time.Duration(cfg.Retention.SlackSamplesDays) * 24 * time.Hour
			if err := db.PruneSlackSamples(slackAge); err != nil {
				slog.Error("prune slack samples", "err", err)
			}
		}
	}
}

// runWindowsLoop calls UpdateWindows on a 30-second cadence so windows
// progress (open the next 5h/weekly window, correct baselines from
// snapshots) even when no HTTP traffic is arriving.
func runWindowsLoop(wg *sync.WaitGroup, stop <-chan struct{}, srv *server.Server) {
	defer wg.Done()
	we := srv.WindowsEngine()
	ticker := time.NewTicker(windowsTickInterval)
	defer ticker.Stop()
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
}

// setupLogging installs slog's default handler. The base destination is a
// rotating JSON file when cfg.Logging.File is non-empty, otherwise a text
// handler on stderr. In both cases the base handler is wrapped by
// feedback.Handler so warn-and-above records are teed into the in-memory
// feedback buffer that GET /api/feedback surfaces on the dashboard — the tray
// app runs with no console, so this is the only way a user sees warnings like
// "price table file not found". Returns the rotating file's Close function
// (nil when stderr is the destination).
func setupLogging(file, level string) *rotatingWriter {
	lvl := parseLogLevel(level)
	var w *rotatingWriter
	var base slog.Handler
	if file != "" {
		rw, err := newRotatingWriter(file, logRotateMaxSize, logRotateMaxBackups)
		if err != nil {
			// The default handler is still active here, so this warning is
			// visible on stderr but predates the tee — acceptable for a
			// one-off startup failure.
			slog.Warn("failed to set up rotating log file, falling back to stderr", "path", file, "err", err)
		} else {
			w = rw
			base = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
		}
	}
	if base == nil {
		base = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	}
	tee := feedback.NewHandler(base, feedback.DefaultBuffer())
	slog.SetDefault(slog.New(tee))
	return w
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
