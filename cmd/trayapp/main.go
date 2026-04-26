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

	"github.com/anthropics/usage-dashboard/internal/config"
	"github.com/anthropics/usage-dashboard/internal/ingest"
	"github.com/anthropics/usage-dashboard/internal/netbind"
	"github.com/anthropics/usage-dashboard/internal/server"
	"github.com/anthropics/usage-dashboard/internal/slack"
	"github.com/anthropics/usage-dashboard/internal/store"
)

// pauseToggle adapts a *slack.Calculator into the tiny `interface{ Toggle() }`
// the tray UI consumes, so the Pause menu item flips the same in-memory
// pause flag the HTTP handlers read.
type pauseToggle struct{ c *slack.Calculator }

func (p pauseToggle) Toggle() { p.c.SetPaused(!p.c.IsPaused()) }

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
	defer db.Close()

	slog.Info("Claude Usage Dashboard starting", "version", Version, "db", cfg.Database.Path)

	srv := server.New(db, cfg)

	priceTable := ingest.LoadPriceTable(cfg.Pricing.TablePath)

	tailer := ingest.NewTailer(cfg.Claude.ProjectsDir, db, priceTable)
	tailer.Start()
	srv.SetTailer(tailer)

	// stop signals every background loop (retention pruner, windows ticker)
	// to exit; wg lets shutdown wait for them. The tailer has its own
	// stopChan + doneChan and is stopped via tailer.Stop() so we don't
	// double-track it on the WaitGroup.
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
	trayDone := make(chan struct{})
	go func() {
		StartTray(trayCtx, srv, pauseToggle{c: srv.SlackCalculator()})
		close(trayDone)
	}()

	// Resolve bind addresses (loopback + detected Docker/WSL adapters + overrides).
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Warn("failed to enumerate network interfaces", "err", err)
		ifaces = nil
	}
	bindAddrs, err := netbind.SelectBindAddrs(ifaces, netbind.BindConfig{
		UserOverrides:  cfg.HTTP.Bind,
		EnableFallback: cfg.HTTP.EnableFallback,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to select bind addresses: %v\n", err)
		os.Exit(1)
	}

	serverErr := make(chan error, len(bindAddrs))

	for _, host := range bindAddrs {
		addr := fmt.Sprintf("%s:%d", host, cfg.HTTP.Port)
		go func(a string) {
			slog.Info("starting HTTP server", "addr", a)
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

	shutdownDone := make(chan struct{})
	go func() {
		slog.Info("shutting down gracefully")
		close(stop)
		cancelTray()
		tailer.Stop()
		wg.Wait()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		slog.Info("shutdown complete")
	case <-time.After(5 * time.Second):
		slog.Warn("shutdown timeout, forcing exit")
	}
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

// setupLogging swaps slog's default handler to write to a rotating file when
// cfg.Logging.File is non-empty. Returns the file's Close function (nil when
// stdout is the destination).
func setupLogging(file, level string) *rotatingWriter {
	if file == "" {
		return nil
	}
	w, err := newRotatingWriter(file, logRotateMaxSize, logRotateMaxBackups)
	if err != nil {
		slog.Warn("failed to set up rotating log file, falling back to stdout", "path", file, "err", err)
		return nil
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: parseLogLevel(level)})
	slog.SetDefault(slog.New(handler))
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
