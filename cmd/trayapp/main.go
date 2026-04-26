package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/anthropics/usage-dashboard/internal/config"
	"github.com/anthropics/usage-dashboard/internal/netbind"
	"github.com/anthropics/usage-dashboard/internal/server"
	"github.com/anthropics/usage-dashboard/internal/store"
)

const Version = "0.0.1"

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.String("version", Version, "show version")
	flag.Parse()

	// Resolve config path if not provided
	if *configPath == "" {
		*configPath = config.ResolveConfigPath()
	}

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Use resolved paths if not set in config
	if cfg.Database.Path == "" {
		cfg.Database.Path = config.ResolveDBPath()
	}
	if cfg.Claude.ProjectsDir == "" {
		cfg.Claude.ProjectsDir = config.ResolveProjectsDir()
	}

	// Ensure database directory exists
	dbDir := filepath.Dir(cfg.Database.Path)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create database directory: %v\n", err)
		os.Exit(1)
	}

	// Open database
	db, err := store.Open(cfg.Database.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	slog.Info("Claude Usage Dashboard starting", "version", Version, "db", cfg.Database.Path)

	// Create HTTP server
	srv := server.New(db, cfg)

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

	// Server error channel — one slot per listener.
	serverErr := make(chan error, len(bindAddrs))

	// Start a listener per resolved address.
	for _, host := range bindAddrs {
		addr := fmt.Sprintf("%s:%d", host, cfg.HTTP.Port)
		go func(a string) {
			slog.Info("starting HTTP server", "addr", a)
			serverErr <- srv.ListenAndServe(a)
		}(addr)
	}

	// Wait for signal, or for every listener to exit. A single listener
	// failing (e.g. bind conflict between 0.0.0.0 and a specific address when
	// fallback is enabled, or a stale interface IP) must not bring down the
	// trayapp while other listeners are still serving.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	remaining := len(bindAddrs)
waitLoop:
	for {
		select {
		case <-sigChan:
			slog.Info("received shutdown signal")
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

	// Graceful shutdown with timeout
	shutdownDone := make(chan struct{})
	go func() {
		slog.Info("shutting down gracefully")
		// Note: http.Server doesn't have built-in Shutdown in this version
		// In production, would need to implement proper shutdown logic
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		slog.Info("shutdown complete")
	case <-time.After(5 * time.Second):
		slog.Warn("shutdown timeout, forcing exit")
	}
}
